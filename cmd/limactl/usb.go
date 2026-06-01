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
	hosts, err := usbip.List()
	if err != nil {
		return fmt.Errorf("enumerating host USB devices: %w", err)
	}

	var (
		allow    []usbip.AllowEntry
		attached map[string]bool
	)
	if len(args) > 0 {
		ctx := cmd.Context()
		inst, err := requireVZUSBInstance(ctx, args[0])
		if err != nil {
			return err
		}
		if allow, err = usbip.ReadAllowlist(inst.Dir); err != nil {
			return err
		}
		if attached, err = guestAttached(ctx, inst); err != nil {
			logrus.WithError(err).Debug("usb: could not query guest attach state")
		}
	}

	w := tabwriter.NewWriter(cmd.OutOrStdout(), 4, 8, 4, ' ', 0)
	fmt.Fprintln(w, "BUSID\tVID:PID\tNAME\tALLOWED\tATTACHED")
	for _, h := range hosts {
		entry := usbip.AllowEntry{
			VendorID:  fmt.Sprintf("%04x", h.Vendor),
			ProductID: fmt.Sprintf("%04x", h.Product),
			BusAddr:   h.Busid,
		}
		allowed := "-"
		att := "-"
		name := ""
		if len(args) > 0 {
			allowed = boolMark(usbip.Allowed(allow, entry))
			att = boolMark(attached[strings.ToLower(fmt.Sprintf("%04x:%04x", h.Vendor, h.Product))])
			name = allowlistName(allow, entry)
		}
		fmt.Fprintf(w, "%s\t%04x:%04x\t%s\t%s\t%s\n", h.Busid, h.Vendor, h.Product, name, allowed, att)
	}
	return w.Flush()
}

func allowlistName(list []usbip.AllowEntry, dev usbip.AllowEntry) string {
	for _, e := range list {
		if usbip.Allowed([]usbip.AllowEntry{e}, dev) {
			return e.Name
		}
	}
	return ""
}

func boolMark(b bool) string {
	if b {
		return "yes"
	}
	return "no"
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

	out, err := runGuestCommand(ctx, inst, fmt.Sprintf("sudo usbip attach -r 127.0.0.1 -b %s", dev.Busid))
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

	port, err := guestPortFor(ctx, inst, dev.Vendor, dev.Product)
	if err != nil {
		return err
	}
	out, err := runGuestCommand(ctx, inst, fmt.Sprintf("sudo usbip detach -p %s", port))
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

var usbPortRe = regexp.MustCompile(`(?m)^Port\s+(\d+):`)

// guestPortFor resolves the vhci port number of an attached device by matching
// its vid:pid in the `usbip port` output.
func guestPortFor(ctx context.Context, inst *limatype.Instance, vendor, product uint16) (string, error) {
	out, err := runGuestCommand(ctx, inst, "usbip port")
	if err != nil {
		return "", fmt.Errorf("usbip port in guest failed: %w\n%s", err, out)
	}
	idTag := fmt.Sprintf("%04x:%04x", vendor, product)
	lines := strings.Split(out, "\n")
	curPort := ""
	for _, line := range lines {
		if m := usbPortRe.FindStringSubmatch(line); m != nil {
			curPort = m[1]
		}
		if curPort != "" && strings.Contains(strings.ToLower(line), idTag) {
			return curPort, nil
		}
	}
	return "", fmt.Errorf("device %s is not attached in the guest", idTag)
}

// guestAttached returns the set of "vid:pid" currently attached in the guest.
func guestAttached(ctx context.Context, inst *limatype.Instance) (map[string]bool, error) {
	out, err := runGuestCommand(ctx, inst, "usbip port")
	if err != nil {
		return nil, err
	}
	re := regexp.MustCompile(`([0-9a-fA-F]{4}):([0-9a-fA-F]{4})`)
	set := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(out, -1) {
		set[strings.ToLower(m[1]+":"+m[2])] = true
	}
	return set, nil
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
