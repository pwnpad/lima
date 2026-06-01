//go:build darwin && !no_vz

// SPDX-FileCopyrightText: Copyright The Lima Authors
// SPDX-License-Identifier: Apache-2.0

package vz

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"

	"github.com/sirupsen/logrus"

	"github.com/lima-vm/lima/v2/pkg/limatype"
	"github.com/lima-vm/lima/v2/pkg/usbip"
)

// usbipVsockPort is the guest-facing vsock port the host USB/IP server listens
// on. The guest bridges its local TCP usbip client to this port (see the
// cidata usbip boot script).
const usbipVsockPort = 2223

// startUSBIPServer opens the configured host USB devices and serves them to the
// guest over vsock using the USB/IP protocol. It is a no-op when no usbDevices
// are configured. The server and opened devices are torn down when ctx ends.
func (m *virtualMachineWrapper) startUSBIPServer(ctx context.Context, inst *limatype.Instance) error {
	if len(inst.Config.USBDevices) == 0 {
		return nil
	}

	var devices []usbip.Device
	for _, d := range inst.Config.USBDevices {
		vid, err := strconv.ParseUint(d.VendorID, 16, 16)
		if err != nil {
			logrus.WithError(err).Warnf("usbip: invalid vendorID %q, skipping", d.VendorID)
			continue
		}
		pid, err := strconv.ParseUint(d.ProductID, 16, 16)
		if err != nil {
			logrus.WithError(err).Warnf("usbip: invalid productID %q, skipping", d.ProductID)
			continue
		}
		dev, err := usbip.Open(uint16(vid), uint16(pid), d.BusAddr)
		if err != nil {
			logrus.WithError(err).Warnf("usbip: failed to open device %s:%s, skipping", d.VendorID, d.ProductID)
			continue
		}
		logrus.Infof("usbip: opened host USB device %s:%s (%s)", d.VendorID, d.ProductID, dev.Info().Busid)
		devices = append(devices, dev)
	}
	if len(devices) == 0 {
		return errors.New("no configured USB devices could be opened for passthrough")
	}

	socketDevices := m.SocketDevices()
	if len(socketDevices) == 0 {
		closeDevices(devices)
		return errors.New("no vsock device available for USB/IP passthrough")
	}
	listener, err := socketDevices[0].Listen(usbipVsockPort)
	if err != nil {
		closeDevices(devices)
		return fmt.Errorf("listening on vsock port %d: %w", usbipVsockPort, err)
	}

	go func() {
		<-ctx.Done()
		_ = listener.Close()
		closeDevices(devices)
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
				if err := usbip.Serve(ctx, conn, devices); err != nil {
					logrus.WithError(err).Debug("usbip: session ended")
				}
			}()
		}
	}()

	logrus.Infof("usbip: serving %d USB device(s) on vsock port %d", len(devices), usbipVsockPort)
	return nil
}

func closeDevices(devices []usbip.Device) {
	for _, d := range devices {
		_ = d.Close()
	}
}
