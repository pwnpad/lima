// SPDX-FileCopyrightText: Copyright The Lima Authors
// SPDX-License-Identifier: Apache-2.0

package usbip

import (
	"context"
	"errors"
)

var ErrDeviceGone = errors.New("usb device disconnected")

type DeviceInfo struct {
	BusNum             uint32
	DevNum             uint32
	Busid              string
	Path               string
	Speed              uint32
	Vendor             uint16
	Product            uint16
	VendorName         string
	ProductName        string
	BcdDevice          uint16
	Class              uint8
	SubClass           uint8
	Protocol           uint8
	ConfigurationValue uint8
	NumConfigurations  uint8
	Interfaces         []InterfaceInfo
}

type InterfaceInfo struct {
	Class    uint8
	SubClass uint8
	Protocol uint8
}

// Device is the host-side handle the USB/IP server relays guest URBs to,
// implemented by the gousb/libusb backend.
type Device interface {
	Info() DeviceInfo
	// Control performs a control transfer on endpoint 0. setup is the raw
	// 8-byte SETUP packet. Standard SET_CONFIGURATION/SET_INTERFACE requests
	// are handled by the backend to drive interface claiming.
	Control(ctx context.Context, setup [8]byte, data []byte) (int, error)
	// Transfer performs a bulk or interrupt transfer on the given endpoint
	// number. The backend claims the owning interface on demand.
	Transfer(ctx context.Context, ep uint8, in bool, buf []byte) (int, error)
	// Gone reports whether the device has been physically removed from the host bus.
	Gone() bool
	Close() error
}

// Provider supplies the set of devices exportable to the guest.
type Provider interface {
	Devices() ([]DeviceInfo, error)
	Open(busid string) (Device, error)
}
