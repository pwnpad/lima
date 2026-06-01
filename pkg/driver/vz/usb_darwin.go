//go:build darwin && !no_vz

// SPDX-FileCopyrightText: Copyright The Lima Authors
// SPDX-License-Identifier: Apache-2.0

package vz

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/sirupsen/logrus"

	"github.com/lima-vm/lima/v2/pkg/limatype"
	"github.com/lima-vm/lima/v2/pkg/usbip"
)

// usbipVsockPort is the guest-facing vsock port the host USB/IP server listens
// on. The guest bridges its local TCP usbip client to this port (see the
// cidata usbip boot script).
const usbipVsockPort = 2223

// startUSBIPServer serves host USB devices to the guest over vsock using the
// USB/IP protocol. It runs whenever vz USB is enabled (usb: true) or any
// usbDevices are configured, so a later live attach has transport ready. The
// exportable set is the instance allowlist file, re-read per request and opened
// lazily at import time; usbDevices seeds that file at start.
func (m *virtualMachineWrapper) startUSBIPServer(ctx context.Context, inst *limatype.Instance) error {
	enabled := (inst.Config.USB != nil && *inst.Config.USB) || len(inst.Config.USBDevices) > 0
	if !enabled {
		return nil
	}

	if err := seedAllowlist(inst); err != nil {
		return fmt.Errorf("seeding usbip allowlist: %w", err)
	}
	provider := usbip.NewProvider(inst.Dir)

	socketDevices := m.SocketDevices()
	if len(socketDevices) == 0 {
		return errors.New("no vsock device available for USB/IP passthrough")
	}
	listener, err := socketDevices[0].Listen(usbipVsockPort)
	if err != nil {
		return fmt.Errorf("listening on vsock port %d: %w", usbipVsockPort, err)
	}

	// Tie the server lifetime to a cancelable context so the VM stop handler can
	// release the vsock listener and any open host USB device handles before the
	// driver process exits (graceful teardown rather than relying on exit alone).
	ctx, cancel := context.WithCancel(ctx)
	m.mu.Lock()
	m.usbipCancel = cancel
	m.mu.Unlock()

	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				if errors.Is(err, net.ErrClosed) {
					return
				}
				logrus.WithError(err).Warn("usbip: vsock accept error")
				return
			}
			go func() {
				defer conn.Close()
				// Close the conn on cancel so a session blocked reading URBs
				// unblocks and releases its host USB device handle.
				go func() {
					<-ctx.Done()
					_ = conn.Close()
				}()
				if err := usbip.Serve(ctx, conn, provider); err != nil {
					logrus.WithError(err).Debug("usbip: session ended")
				}
			}()
		}
	}()

	logrus.Infof("usbip: serving allowlisted USB devices on vsock port %d", usbipVsockPort)
	return nil
}

// seedAllowlist merges the YAML usbDevices into the instance allowlist file so
// configured devices are exportable from boot. Live attaches append to the same
// file later. Existing entries (e.g. from a prior live attach) are preserved.
func seedAllowlist(inst *limatype.Instance) error {
	if len(inst.Config.USBDevices) == 0 {
		return nil
	}
	list, err := usbip.ReadAllowlist(inst.Dir)
	if err != nil {
		return err
	}
	for _, d := range inst.Config.USBDevices {
		list = usbip.AddEntry(list, usbip.AllowEntry{
			Name:      d.Name,
			VendorID:  d.VendorID,
			ProductID: d.ProductID,
			BusAddr:   d.BusAddr,
		})
	}
	return usbip.WriteAllowlist(inst.Dir, list)
}
