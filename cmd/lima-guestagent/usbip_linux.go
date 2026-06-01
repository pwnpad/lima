// SPDX-FileCopyrightText: Copyright The Lima Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/lima-vm/lima/v2/pkg/usbip"
)

func newUsbipCommand() *cobra.Command {
	usbipCommand := &cobra.Command{
		Use:   "usbip",
		Short: "Import host USB devices over vsock onto vhci-hcd (used by limactl usb)",
	}
	usbipCommand.AddCommand(
		newUsbipAttachCommand(),
		newUsbipDetachCommand(),
		newUsbipPortCommand(),
	)
	return usbipCommand
}

func newUsbipAttachCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "attach",
		Short: "Import a host USB device (--busid) or all advertised devices (--all)",
		RunE:  usbipAttachAction,
	}
	cmd.Flags().String("busid", "", "host busid to import")
	cmd.Flags().Bool("all", false, "import every device the host advertises")
	return cmd
}

func usbipAttachAction(cmd *cobra.Command, _ []string) error {
	all, _ := cmd.Flags().GetBool("all")
	busid, _ := cmd.Flags().GetString("busid")
	switch {
	case all:
		return usbip.AttachAll()
	case busid != "":
		return usbip.AttachBusid(busid)
	default:
		return errors.New("specify --busid or --all")
	}
}

func newUsbipDetachCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "detach",
		Short: "Detach an imported device by vendor:product",
		RunE:  usbipDetachAction,
	}
	cmd.Flags().String("vidpid", "", "device to detach, as vendor:product hex (e.g. 0403:6015)")
	return cmd
}

func usbipDetachAction(cmd *cobra.Command, _ []string) error {
	vidpid, _ := cmd.Flags().GetString("vidpid")
	if vidpid == "" {
		return errors.New("specify --vidpid")
	}
	return usbip.DetachVIDPID(vidpid)
}

func newUsbipPortCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "port",
		Short: "List vendor:product of devices currently imported onto vhci-hcd",
		RunE:  usbipPortAction,
	}
}

func usbipPortAction(cmd *cobra.Command, _ []string) error {
	ids, err := usbip.AttachedVIDPIDs()
	if err != nil {
		return err
	}
	for _, id := range ids {
		fmt.Fprintln(cmd.OutOrStdout(), id)
	}
	return nil
}
