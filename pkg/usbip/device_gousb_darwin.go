// SPDX-FileCopyrightText: Copyright The Lima Authors
// SPDX-License-Identifier: Apache-2.0

//go:build darwin && cgo

package usbip

import (
	"context"
	"fmt"
	"sync"

	"github.com/google/gousb"
)

// USB/IP speed codes (see linux/usbip.h: enum usb_device_speed mapping).
const (
	usbipSpeedUnknown = 0
	usbipSpeedLow     = 1
	usbipSpeedFull    = 2
	usbipSpeedHigh    = 3
	usbipSpeedSuper   = 5
)

// Standard control-request constants used to intercept configuration changes.
const (
	reqSetConfiguration = 0x09
	reqSetInterface     = 0x0b
)

type gousbDevice struct {
	gctx *gousb.Context
	dev  *gousb.Device
	info DeviceInfo

	mu     sync.Mutex
	cfg    *gousb.Config
	ifaces map[int]*gousb.Interface
	inEps  map[uint8]*gousb.InEndpoint
	outEps map[uint8]*gousb.OutEndpoint
}

// Open finds and opens the host USB device matching vendorID/productID (and, if
// non-empty, the "<bus>-<addr>" busAddr), returning a USB/IP-servable handle.
func Open(vendorID, productID uint16, busAddr string) (Device, error) {
	gctx := gousb.NewContext()
	devs, err := gctx.OpenDevices(func(d *gousb.DeviceDesc) bool {
		if uint16(d.Vendor) != vendorID || uint16(d.Product) != productID {
			return false
		}
		if busAddr != "" && busidString(d.Bus, d.Address) != busAddr {
			return false
		}
		return true
	})
	if len(devs) == 0 {
		_ = gctx.Close()
		if err != nil {
			return nil, fmt.Errorf("opening usb device %04x:%04x: %w", vendorID, productID, err)
		}
		return nil, fmt.Errorf("usb device %04x:%04x not found", vendorID, productID)
	}
	// Keep the first match; close any extras.
	dev := devs[0]
	for _, extra := range devs[1:] {
		_ = extra.Close()
	}

	// Do NOT enable gousb autodetach: on macOS libusb_detach_kernel_driver
	// triggers a whole-device IOKit capture that needs root or the
	// com.apple.vm.device-access entitlement, and gousb's Config() detaches
	// every interface unconditionally. Driverless devices (no kernel driver
	// bound to their interfaces) can be claimed without it; devices that a
	// macOS driver holds will fail claim with LIBUSB_ERROR_BUSY and need a
	// privileged host server.

	g := &gousbDevice{
		gctx:   gctx,
		dev:    dev,
		info:   buildInfo(dev),
		ifaces: map[int]*gousb.Interface{},
		inEps:  map[uint8]*gousb.InEndpoint{},
		outEps: map[uint8]*gousb.OutEndpoint{},
	}
	return g, nil
}

// allowEntryFor converts a host DeviceInfo into the identity form used to match
// against the allowlist (vendor/product hex plus busid for disambiguation).
func allowEntryFor(info DeviceInfo) AllowEntry {
	return AllowEntry{
		VendorID:  fmt.Sprintf("%04x", info.Vendor),
		ProductID: fmt.Sprintf("%04x", info.Product),
		BusAddr:   info.Busid,
	}
}

// allowlistProvider serves only the host devices permitted by the instance's
// allowlist file, re-read on every call so live changes take effect without a
// restart. It opens devices lazily, at import time.
type allowlistProvider struct {
	instDir string
}

// NewProvider returns a Provider backed by the instance's USB/IP allowlist file.
func NewProvider(instDir string) Provider {
	return &allowlistProvider{instDir: instDir}
}

func (p *allowlistProvider) Devices() ([]DeviceInfo, error) {
	allow, err := ReadAllowlist(p.instDir)
	if err != nil {
		return nil, err
	}
	if len(allow) == 0 {
		return nil, nil
	}
	hosts, err := List()
	if err != nil {
		return nil, err
	}
	var out []DeviceInfo
	for _, info := range hosts {
		if Allowed(allow, allowEntryFor(info)) {
			out = append(out, info)
		}
	}
	return out, nil
}

func (p *allowlistProvider) Open(busid string) (Device, error) {
	allow, err := ReadAllowlist(p.instDir)
	if err != nil {
		return nil, err
	}
	hosts, err := List()
	if err != nil {
		return nil, err
	}
	for _, info := range hosts {
		if info.Busid != busid {
			continue
		}
		if !Allowed(allow, allowEntryFor(info)) {
			return nil, fmt.Errorf("usb device %s not permitted by allowlist", busid)
		}
		return Open(info.Vendor, info.Product, info.Busid)
	}
	return nil, fmt.Errorf("usb device %s not found on host", busid)
}

func mapSpeed(s gousb.Speed) uint32 {
	switch s {
	case gousb.SpeedLow:
		return usbipSpeedLow
	case gousb.SpeedFull:
		return usbipSpeedFull
	case gousb.SpeedHigh:
		return usbipSpeedHigh
	case gousb.SpeedSuper:
		return usbipSpeedSuper
	default:
		return usbipSpeedUnknown
	}
}

// List enumerates the USB devices currently present on the host without
// claiming them. The OpenDevices filter returns false for every device, so each
// is inspected (via its descriptor) but never opened.
func List() ([]DeviceInfo, error) {
	gctx := gousb.NewContext()
	defer gctx.Close()
	var out []DeviceInfo
	if _, err := gctx.OpenDevices(func(d *gousb.DeviceDesc) bool {
		out = append(out, infoFromDesc(d)) // returning false: never opened
		return false
	}); err != nil {
		return out, err
	}
	return out, nil
}

func buildInfo(dev *gousb.Device) DeviceInfo {
	info := infoFromDesc(dev.Desc)
	if n, err := dev.ActiveConfigNum(); err == nil {
		info.ConfigurationValue = uint8(n)
	}
	return info
}

func infoFromDesc(d *gousb.DeviceDesc) DeviceInfo {
	info := DeviceInfo{
		BusNum:            uint32(d.Bus),
		DevNum:            uint32(d.Address),
		Busid:             busidString(d.Bus, d.Address),
		Path:              fmt.Sprintf("/sys/devices/lima/usb/%s", busidString(d.Bus, d.Address)),
		Speed:             mapSpeed(d.Speed),
		Vendor:            uint16(d.Vendor),
		Product:           uint16(d.Product),
		BcdDevice:         uint16(d.Device),
		Class:             uint8(d.Class),
		SubClass:          uint8(d.SubClass),
		Protocol:          uint8(d.Protocol),
		NumConfigurations: uint8(len(d.Configs)),
	}
	// Advertise the interfaces of the active (or first) configuration.
	cfgNum := int(info.ConfigurationValue)
	cfg, ok := d.Configs[cfgNum]
	if !ok {
		for _, c := range d.Configs {
			cfg = c
			break
		}
	}
	for _, intf := range cfg.Interfaces {
		if len(intf.AltSettings) == 0 {
			continue
		}
		alt := intf.AltSettings[0]
		info.Interfaces = append(info.Interfaces, InterfaceInfo{
			Class:    uint8(alt.Class),
			SubClass: uint8(alt.SubClass),
			Protocol: uint8(alt.Protocol),
		})
	}
	return info
}

func (g *gousbDevice) Info() DeviceInfo {
	return g.info
}

func (g *gousbDevice) Control(_ context.Context, setup [8]byte, data []byte) (int, error) {
	rType, request, value, index, _ := controlSetup(setup)

	// Drive configuration/interface state through gousb rather than passing the
	// raw control transfer to libusb, which tracks this state itself.
	if rType == 0x00 && request == reqSetConfiguration {
		return 0, g.setConfiguration(int(value))
	}
	if rType == 0x01 && request == reqSetInterface {
		return 0, g.setInterface(int(index), int(value))
	}

	return g.dev.Control(rType, request, value, index, data)
}

func (g *gousbDevice) setConfiguration(num int) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.releaseLocked()
	cfg, err := g.dev.Config(num)
	if err != nil {
		return fmt.Errorf("set configuration %d: %w", num, err)
	}
	g.cfg = cfg
	return nil
}

func (g *gousbDevice) setInterface(num, alt int) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.ensureConfigLocked(); err != nil {
		return err
	}
	if old, ok := g.ifaces[num]; ok {
		old.Close()
		delete(g.ifaces, num)
		g.dropEndpointsLocked(num)
	}
	intf, err := g.cfg.Interface(num, alt)
	if err != nil {
		return fmt.Errorf("set interface %d alt %d: %w", num, alt, err)
	}
	g.ifaces[num] = intf
	return nil
}

func (g *gousbDevice) Transfer(ctx context.Context, ep uint8, in bool, buf []byte) (int, error) {
	epNum := ep & 0x0f
	if in {
		inEp, err := g.inEndpoint(epNum)
		if err != nil {
			return 0, err
		}
		return inEp.ReadContext(ctx, buf)
	}
	outEp, err := g.outEndpoint(epNum)
	if err != nil {
		return 0, err
	}
	return outEp.WriteContext(ctx, buf)
}

func (g *gousbDevice) inEndpoint(epNum uint8) (*gousb.InEndpoint, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if e, ok := g.inEps[epNum]; ok {
		return e, nil
	}
	intf, err := g.claimEndpointInterfaceLocked(epNum, true)
	if err != nil {
		return nil, err
	}
	e, err := intf.InEndpoint(int(epNum))
	if err != nil {
		return nil, fmt.Errorf("open in endpoint %d: %w", epNum, err)
	}
	g.inEps[epNum] = e
	return e, nil
}

func (g *gousbDevice) outEndpoint(epNum uint8) (*gousb.OutEndpoint, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if e, ok := g.outEps[epNum]; ok {
		return e, nil
	}
	intf, err := g.claimEndpointInterfaceLocked(epNum, false)
	if err != nil {
		return nil, err
	}
	e, err := intf.OutEndpoint(int(epNum))
	if err != nil {
		return nil, fmt.Errorf("open out endpoint %d: %w", epNum, err)
	}
	g.outEps[epNum] = e
	return e, nil
}

// claimEndpointInterfaceLocked finds the interface owning the bulk/interrupt
// endpoint with number epNum and direction, claiming it if not already held.
func (g *gousbDevice) claimEndpointInterfaceLocked(epNum uint8, in bool) (*gousb.Interface, error) {
	if err := g.ensureConfigLocked(); err != nil {
		return nil, err
	}
	wantDir := gousb.EndpointDirectionOut
	if in {
		wantDir = gousb.EndpointDirectionIn
	}
	for _, intf := range g.cfg.Desc.Interfaces {
		for _, alt := range intf.AltSettings {
			for _, epDesc := range alt.Endpoints {
				if epDesc.Number != int(epNum) || epDesc.Direction != wantDir {
					continue
				}
				if epDesc.TransferType == gousb.TransferTypeIsochronous {
					return nil, fmt.Errorf("endpoint %d is isochronous (unsupported)", epNum)
				}
				if claimed, ok := g.ifaces[intf.Number]; ok {
					return claimed, nil
				}
				claimed, err := g.cfg.Interface(intf.Number, alt.Alternate)
				if err != nil {
					return nil, fmt.Errorf("claim interface %d: %w", intf.Number, err)
				}
				g.ifaces[intf.Number] = claimed
				return claimed, nil
			}
		}
	}
	return nil, fmt.Errorf("no interface provides endpoint %d (in=%v)", epNum, in)
}

func (g *gousbDevice) ensureConfigLocked() error {
	if g.cfg != nil {
		return nil
	}
	num := int(g.info.ConfigurationValue)
	if num == 0 {
		num = 1
	}
	cfg, err := g.dev.Config(num)
	if err != nil {
		return fmt.Errorf("activate configuration %d: %w", num, err)
	}
	g.cfg = cfg
	return nil
}

func (g *gousbDevice) dropEndpointsLocked(intfNum int) {
	// Endpoints belonging to the released interface can no longer be used; drop
	// the cache entries so they are re-opened against a freshly claimed interface.
	if g.cfg == nil {
		return
	}
	for _, intf := range g.cfg.Desc.Interfaces {
		if intf.Number != intfNum {
			continue
		}
		for _, alt := range intf.AltSettings {
			for _, epDesc := range alt.Endpoints {
				delete(g.inEps, uint8(epDesc.Number))
				delete(g.outEps, uint8(epDesc.Number))
			}
		}
	}
}

func (g *gousbDevice) releaseLocked() {
	for _, intf := range g.ifaces {
		intf.Close()
	}
	g.ifaces = map[int]*gousb.Interface{}
	g.inEps = map[uint8]*gousb.InEndpoint{}
	g.outEps = map[uint8]*gousb.OutEndpoint{}
	if g.cfg != nil {
		_ = g.cfg.Close()
		g.cfg = nil
	}
}

func (g *gousbDevice) Close() error {
	g.mu.Lock()
	g.releaseLocked()
	g.mu.Unlock()
	err := g.dev.Close()
	if cerr := g.gctx.Close(); cerr != nil && err == nil {
		err = cerr
	}
	return err
}
