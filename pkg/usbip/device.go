// SPDX-FileCopyrightText: Copyright The Lima Authors
// SPDX-License-Identifier: Apache-2.0

package usbip

import (
	"context"
	"errors"
)

// ErrDeviceGone is wrapped by a backend Device's transfer methods when the
// physical device has been disconnected from the host. The server uses it to
// tear the session down (dropping the vsock socket) so the guest's vhci-hcd
// releases the port instead of leaving a dead import behind.
var ErrDeviceGone = errors.New("usb device disconnected")

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
	// Gone reports whether the device has been physically removed from the host
	// bus. It distinguishes a genuine unplug from a device that is merely
	// unresponsive (still enumerated), so the server only tears the session down
	// for a real disconnect.
	Gone() bool
	// Close releases the device and any claimed interfaces.
	Close() error
}

// Provider supplies the set of devices a single USB/IP session may see. It is
// consulted per connection so runtime changes to the allowlist take effect
// without restarting the server.
type Provider interface {
	// Devices returns the devices currently exportable to the guest (present on
	// the host and permitted by the allowlist).
	Devices() ([]DeviceInfo, error)
	// Open opens the exportable device identified by busid for one session.
	Open(busid string) (Device, error)
}
