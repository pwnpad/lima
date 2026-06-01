// SPDX-FileCopyrightText: Copyright The Lima Authors
// SPDX-License-Identifier: Apache-2.0

package usbip

import "context"

// DeviceInfo describes a USB device well enough to populate the USB/IP
// OP_REP_DEVLIST and OP_REP_IMPORT replies.
type DeviceInfo struct {
	BusNum             uint32
	DevNum             uint32
	Busid              string
	Path               string
	Speed              uint32
	Vendor             uint16
	Product            uint16
	BcdDevice          uint16
	Class              uint8
	SubClass           uint8
	Protocol           uint8
	ConfigurationValue uint8
	NumConfigurations  uint8
	Interfaces         []InterfaceInfo
}

// InterfaceInfo describes a single USB interface for OP_REP_DEVLIST.
type InterfaceInfo struct {
	Class    uint8
	SubClass uint8
	Protocol uint8
}

// Device is the host-side handle the USB/IP server relays guest URBs to. It is
// implemented by the gousb/libusb backend (device_gousb_darwin.go), and kept as
// an interface so the protocol/server logic stays platform-independent and
// testable.
type Device interface {
	// Info returns the descriptors used to advertise the device to the guest.
	Info() DeviceInfo
	// Control performs a control transfer on endpoint 0. setup is the raw
	// 8-byte SETUP packet. For IN transfers, data is the buffer to fill and the
	// returned int is the number of bytes read; for OUT transfers, data holds
	// the payload to send. Standard SET_CONFIGURATION/SET_INTERFACE requests are
	// handled by the backend to drive interface claiming.
	Control(ctx context.Context, setup [8]byte, data []byte) (int, error)
	// Transfer performs a bulk or interrupt transfer on the given endpoint
	// number. For IN (dirIn), buf is filled and the byte count returned; for OUT,
	// buf is sent. The backend claims the owning interface on demand.
	Transfer(ctx context.Context, ep uint8, in bool, buf []byte) (int, error)
	// Close releases the device and any claimed interfaces.
	Close() error
}
