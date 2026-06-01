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

const usbipVsockPort = 2223

// startUSBIPServer serves allowed host USB devices to the guest over vsock.
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
