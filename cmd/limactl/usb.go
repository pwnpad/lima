//go:build darwin && cgo

// SPDX-FileCopyrightText: Copyright The Lima Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"

	"github.com/lima-vm/lima/v2/pkg/limatype"
	"github.com/lima-vm/lima/v2/pkg/sshutil"
	"github.com/lima-vm/lima/v2/pkg/store"
	"github.com/lima-vm/lima/v2/pkg/usbip"
)

func newUsbCommand() *cobra.Command {
	usbCommand := &cobra.Command{
		Use:   "usb",
		Short: "Live USB passthrough management (vz only)",
		Example: `  List host USB devices and their attach state:
  $ limactl usb list INSTANCE

  Attach a host USB device to a running instance:
  $ limactl usb attach INSTANCE --vendor 0bda --product 8812

  Detach it again:
  $ limactl usb detach INSTANCE --vendor 0bda --product 8812`,
		SilenceUsage:  true,
		SilenceErrors: true,
		GroupID:       advancedCommand,
	}
	usbCommand.AddCommand(
		newUsbListCommand(),
		newUsbAttachCommand(),
		newUsbDetachCommand(),
	)
	return usbCommand
}

func newUsbListCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:               "list [INSTANCE]",
		Aliases:           []string{"ls"},
		Short:             "List host USB devices (annotated with allow/attach state when INSTANCE is given)",
		Args:              cobra.MaximumNArgs(1),
		RunE:              usbListAction,
		ValidArgsFunction: usbBashComplete,
	}
	return cmd
}

func newUsbAttachCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:               "attach INSTANCE [BUSID]",
		Short:             "Attach a host USB device to a running vz instance",
		Args:              cobra.RangeArgs(1, 2),
		RunE:              usbAttachAction,
		ValidArgsFunction: usbBashComplete,
	}
	cmd.Flags().String("vendor", "", "vendor ID in hex, e.g. 0bda")
	cmd.Flags().String("product", "", "product ID in hex, e.g. 8812")
	cmd.Flags().String("bus-addr", "", "host bus-address (e.g. 20-3) to disambiguate identical devices")
	cmd.Flags().String("name", "", "friendly name recorded in the allowlist")
	return cmd
}

func newUsbDetachCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:               "detach INSTANCE [BUSID]",
		Short:             "Detach a USB device from a running vz instance",
		Args:              cobra.RangeArgs(1, 2),
		RunE:              usbDetachAction,
		ValidArgsFunction: usbBashComplete,
	}
	cmd.Flags().String("vendor", "", "vendor ID in hex, e.g. 0bda")
	cmd.Flags().String("product", "", "product ID in hex, e.g. 8812")
	cmd.Flags().String("bus-addr", "", "host bus-address (e.g. 20-3) to disambiguate identical devices")
	return cmd
}

func usbListAction(cmd *cobra.Command, args []string) error {
	hosts, err := usbip.ListNamed()
	if err != nil {
		return fmt.Errorf("enumerating host USB devices: %w", err)
	}

	ctx := cmd.Context()
	// attachedTo maps "vid:pid" -> the instance that currently has the device
	// imported; names maps it -> the friendly name from that instance's allowlist.
	attachedTo, names, err := usbAttachments(ctx, args)
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(cmd.OutOrStdout(), 4, 8, 4, ' ', 0)
	fmt.Fprintln(w, "BUSID\tVID:PID\tVENDOR\tPRODUCT\tNAME\tATTACHED TO")
	for _, h := range hosts {
		at, nm := attachedTo[h.Busid], names[h.Busid]
		if at == "" { // tolerate imports that predate host-busid reporting
			vidpid := strings.ToLower(fmt.Sprintf("%04x:%04x", h.Vendor, h.Product))
			at, nm = attachedTo[vidpid], names[vidpid]
		}
		fmt.Fprintf(w, "%s\t%04x:%04x\t%s\t%s\t%s\t%s\n",
			h.Busid, h.Vendor, h.Product,
			dashIfEmpty(h.VendorName), dashIfEmpty(h.ProductName),
			dashIfEmpty(nm), dashIfEmpty(at))
	}
	return w.Flush()
}

// usbAttachments scans the candidate instances (a single one when args names it,
// otherwise every instance) and returns, keyed by host busid, the instance
// currently holding each device and its friendly name from that instance's
// allowlist. Keying by busid (rather than vid:pid) lets identical devices be
// told apart.
func usbAttachments(ctx context.Context, args []string) (attachedTo, names map[string]string, err error) {
	attachedTo = map[string]string{}
	names = map[string]string{}

	var candidates []string
	if len(args) > 0 {
		candidates = []string{args[0]}
	} else if candidates, err = store.Instances(); err != nil {
		return nil, nil, fmt.Errorf("listing instances: %w", err)
	}

	for _, name := range candidates {
		inst, err := store.Inspect(ctx, name)
		if err != nil {
			logrus.WithError(err).Debugf("usb: could not inspect instance %q", name)
			continue
		}
		if !isVZUSBRunning(inst) {
			continue
		}
		attached, err := guestAttachedDevices(ctx, inst)
		if err != nil {
			logrus.WithError(err).Debugf("usb: could not query attach state of %q", inst.Name)
			continue
		}
		allow, err := usbip.ReadAllowlist(inst.Dir)
		if err != nil {
			logrus.WithError(err).Debugf("usb: could not read allowlist of %q", inst.Name)
		}
		for busid, vidpid := range attached {
			if _, seen := attachedTo[busid]; seen {
				continue // first match wins; a physical device attaches to one VM
			}
			attachedTo[busid] = inst.Name
			vid, pid, ok := strings.Cut(vidpid, ":")
			if !ok {
				continue
			}
			dev := usbip.AllowEntry{VendorID: vid, ProductID: pid}
			if busid != vidpid { // a real busid, not the vid:pid fallback
				dev.BusAddr = busid
				dev.Busid = busid
			}
			names[busid] = allowlistName(allow, dev)
		}
	}
	return attachedTo, names, nil
}

func allowlistName(list []usbip.AllowEntry, dev usbip.AllowEntry) string {
	for _, e := range list {
		if usbip.Allowed([]usbip.AllowEntry{e}, dev) {
			return e.Name
		}
	}
	return ""
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func usbAttachAction(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	inst, err := requireVZUSBInstance(ctx, args[0])
	if err != nil {
		return err
	}
	flags := cmd.Flags()
	vendor, _ := flags.GetString("vendor")
	product, _ := flags.GetString("product")
	busAddr, _ := flags.GetString("bus-addr")
	name, _ := flags.GetString("name")
	busid := ""
	if len(args) > 1 {
		busid = args[1]
	}

	dev, err := resolveHostDevice(busid, vendor, product, busAddr)
	if err != nil {
		return err
	}

	list, err := usbip.ReadAllowlist(inst.Dir)
	if err != nil {
		return err
	}
	list = usbip.AddEntry(list, usbip.AllowEntry{
		Name:      name,
		VendorID:  fmt.Sprintf("%04x", dev.Vendor),
		ProductID: fmt.Sprintf("%04x", dev.Product),
		BusAddr:   dev.Busid,
		Busid:     dev.Busid,
	})
	if err := usbip.WriteAllowlist(inst.Dir, list); err != nil {
		return err
	}

	out, err := runGuestCommand(ctx, inst, fmt.Sprintf("sudo %s usbip attach --busid %s", guestAgentBin(inst), dev.Busid))
	if err != nil {
		return fmt.Errorf("usbip attach in guest failed: %w\n%s", err, out)
	}
	logrus.Infof("usb: attached %04x:%04x (%s) to instance %q", dev.Vendor, dev.Product, dev.Busid, inst.Name)
	return nil
}

func usbDetachAction(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	inst, err := requireVZUSBInstance(ctx, args[0])
	if err != nil {
		return err
	}
	flags := cmd.Flags()
	vendor, _ := flags.GetString("vendor")
	product, _ := flags.GetString("product")
	busAddr, _ := flags.GetString("bus-addr")
	busid := ""
	if len(args) > 1 {
		busid = args[1]
	}

	dev, err := resolveHostDevice(busid, vendor, product, busAddr)
	if err != nil {
		return err
	}

	out, err := runGuestCommand(ctx, inst, fmt.Sprintf("sudo %s usbip detach --vidpid %04x:%04x", guestAgentBin(inst), dev.Vendor, dev.Product))
	if err != nil {
		return fmt.Errorf("usbip detach in guest failed: %w\n%s", err, out)
	}

	list, err := usbip.ReadAllowlist(inst.Dir)
	if err != nil {
		return err
	}
	list, removed := usbip.RemoveEntry(list, usbip.AllowEntry{
		VendorID:  fmt.Sprintf("%04x", dev.Vendor),
		ProductID: fmt.Sprintf("%04x", dev.Product),
		BusAddr:   dev.Busid,
	})
	if removed {
		if err := usbip.WriteAllowlist(inst.Dir, list); err != nil {
			return err
		}
	}
	logrus.Infof("usb: detached %04x:%04x from instance %q", dev.Vendor, dev.Product, inst.Name)
	return nil
}

// resolveHostDevice locates the host USB device identified either by busid or by
// vendor/product (optionally disambiguated by bus-addr).
func resolveHostDevice(busid, vendor, product, busAddr string) (usbip.DeviceInfo, error) {
	hosts, err := usbip.List()
	if err != nil {
		return usbip.DeviceInfo{}, fmt.Errorf("enumerating host USB devices: %w", err)
	}
	if busid != "" {
		for _, h := range hosts {
			if h.Busid == busid {
				return h, nil
			}
		}
		return usbip.DeviceInfo{}, fmt.Errorf("no host USB device with busid %s", busid)
	}
	if vendor == "" || product == "" {
		return usbip.DeviceInfo{}, fmt.Errorf("specify a BUSID or both --vendor and --product")
	}
	vid, err := strconv.ParseUint(vendor, 16, 16)
	if err != nil {
		return usbip.DeviceInfo{}, fmt.Errorf("invalid --vendor %q: %w", vendor, err)
	}
	pid, err := strconv.ParseUint(product, 16, 16)
	if err != nil {
		return usbip.DeviceInfo{}, fmt.Errorf("invalid --product %q: %w", product, err)
	}
	var matches []usbip.DeviceInfo
	for _, h := range hosts {
		if h.Vendor != uint16(vid) || h.Product != uint16(pid) {
			continue
		}
		if busAddr != "" && h.Busid != busAddr {
			continue
		}
		matches = append(matches, h)
	}
	switch len(matches) {
	case 0:
		return usbip.DeviceInfo{}, fmt.Errorf("no host USB device %04x:%04x found", vid, pid)
	case 1:
		return matches[0], nil
	default:
		return usbip.DeviceInfo{}, fmt.Errorf("multiple %04x:%04x devices on host; disambiguate with --bus-addr", vid, pid)
	}
}

// isVZUSBRunning reports whether the instance is a running vz instance with USB
// passthrough enabled — the precondition for any live USB operation.
func isVZUSBRunning(inst *limatype.Instance) bool {
	if inst.Config.VMType == nil || *inst.Config.VMType != limatype.VZ {
		return false
	}
	enabled := (inst.Config.USB != nil && *inst.Config.USB) || len(inst.Config.USBDevices) > 0
	return enabled && inst.Status == limatype.StatusRunning
}

func requireVZUSBInstance(ctx context.Context, name string) (*limatype.Instance, error) {
	inst, err := store.Inspect(ctx, name)
	if err != nil {
		return nil, err
	}
	if inst.Config.VMType == nil || *inst.Config.VMType != limatype.VZ {
		return nil, fmt.Errorf("instance %q is not a vz instance; USB passthrough is vz only", name)
	}
	enabled := (inst.Config.USB != nil && *inst.Config.USB) || len(inst.Config.USBDevices) > 0
	if !enabled {
		return nil, fmt.Errorf("instance %q does not have USB enabled; set `usb: true` and restart", name)
	}
	if inst.Status != limatype.StatusRunning {
		return nil, fmt.Errorf("instance %q is not running", name)
	}
	return inst, nil
}

// guestAgentBin returns the absolute path to the guest agent binary inside the
// instance, honoring the configured install prefix.
func guestAgentBin(inst *limatype.Instance) string {
	prefix := "/usr/local"
	if inst.Config.GuestInstallPrefix != nil {
		prefix = *inst.Config.GuestInstallPrefix
	}
	return prefix + "/bin/lima-guestagent"
}

// guestAttachedDevices returns, keyed by host busid, the "vid:pid" of every
// device currently imported in the guest (the guest agent's `usbip port` prints
// "vid:pid busid" per line). Lines that predate host-busid reporting carry no
// busid and are keyed by their vid:pid instead, so they still surface.
func guestAttachedDevices(ctx context.Context, inst *limatype.Instance) (map[string]string, error) {
	out, err := runGuestCommand(ctx, inst, guestAgentBin(inst)+" usbip port")
	if err != nil {
		return nil, err
	}
	vidpidRe := regexp.MustCompile(`^([0-9a-fA-F]{4}):([0-9a-fA-F]{4})$`)
	byBusid := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		m := vidpidRe.FindStringSubmatch(fields[0])
		if m == nil {
			continue
		}
		vidpid := strings.ToLower(m[1] + ":" + m[2])
		busid := vidpid
		if len(fields) > 1 {
			busid = fields[1]
		}
		byBusid[busid] = vidpid
	}
	return byBusid, nil
}

func usbBashComplete(cmd *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
	return bashCompleteInstanceNames(cmd)
}

// runGuestCommand runs a single command in the instance over SSH and returns its
// combined output.
func runGuestCommand(ctx context.Context, inst *limatype.Instance, command string) (string, error) {
	sshExe, err := sshutil.NewSSHExe()
	if err != nil {
		return "", err
	}
	sshOpts, err := sshutil.SSHOpts(
		ctx,
		sshExe,
		inst.Dir,
		*inst.Config.User.Name,
		*inst.Config.SSH.LoadDotSSHPubKeys,
		*inst.Config.SSH.ForwardAgent,
		*inst.Config.SSH.ForwardX11,
		*inst.Config.SSH.ForwardX11Trusted)
	if err != nil {
		return "", err
	}
	sshArgs := append([]string{}, sshExe.Args...)
	sshArgs = append(sshArgs, sshutil.SSHArgsFromOpts(sshOpts)...)
	sshArgs = append(sshArgs,
		"-o", "LogLevel=ERROR",
		"-p", strconv.Itoa(inst.SSHLocalPort),
		inst.SSHAddress,
		"--",
		command,
	)
	cmd := exec.CommandContext(ctx, sshExe.Exe, sshArgs...)
	b, err := cmd.CombinedOutput()
	return string(b), err
}
