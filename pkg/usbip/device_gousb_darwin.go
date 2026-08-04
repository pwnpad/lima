// SPDX-FileCopyrightText: Copyright The Lima Authors
// SPDX-License-Identifier: Apache-2.0

//go:build darwin && cgo

package usbip

/*
#cgo LDFLAGS: -framework IOKit -framework CoreFoundation
#include <IOKit/IOKitLib.h>
#include <IOKit/usb/IOUSBLib.h>
#include <IOKit/IOCFPlugIn.h>
#include <CoreFoundation/CoreFoundation.h>
#include <stdlib.h>

#ifndef kIOMainPortDefault
  #ifdef kIOMasterPortDefault
    #define kIOMainPortDefault kIOMasterPortDefault
  #else
    #define kIOMainPortDefault 0
  #endif
#endif

#pragma clang diagnostic push
#pragma clang diagnostic ignored "-Wdeprecated-declarations"

static io_service_t findDeviceByLocationID(uint32_t targetLocationID) {
    CFMutableDictionaryRef matchingDict = IOServiceMatching("IOUSBHostDevice");
    if (!matchingDict) {
        matchingDict = IOServiceMatching("IOUSBDevice");
    }
    if (!matchingDict) return 0;

    io_iterator_t iter = 0;
    kern_return_t kr = IOServiceGetMatchingServices(kIOMainPortDefault, matchingDict, &iter);
    if (kr != KERN_SUCCESS || !iter) return 0;

    io_service_t found = 0;
    io_service_t svc;
    while ((svc = IOIteratorNext(iter)) != 0) {
        CFNumberRef locRef = IORegistryEntryCreateCFProperty(svc,
            CFSTR("locationID"), kCFAllocatorDefault, 0);
        if (locRef) {
            int loc = 0;
            CFNumberGetValue(locRef, kCFNumberIntType, &loc);
            CFRelease(locRef);
            if ((uint32_t)loc == targetLocationID) {
                found = svc;
                IOObjectRelease(iter);
                return found;
            }
        }
        IOObjectRelease(svc);
    }
    IOObjectRelease(iter);
    return 0;
}

static IOUSBDeviceInterface320** openDeviceInterface(io_service_t svc) {
    IOCFPlugInInterface **plugin = NULL;
    SInt32 score;
    kern_return_t kr = IOCreatePlugInInterfaceForService(svc,
        kIOUSBDeviceUserClientTypeID, kIOCFPlugInInterfaceID, &plugin, &score);
    if (kr != KERN_SUCCESS || !plugin) return NULL;

    IOUSBDeviceInterface320 **iface = NULL;
    HRESULT res = (*plugin)->QueryInterface(plugin,
        CFUUIDGetUUIDBytes(kIOUSBDeviceInterfaceID320), (LPVOID *)&iface);
    (*plugin)->Release(plugin);
    return (res == 0 && iface) ? iface : NULL;
}

static int deviceOpen(IOUSBDeviceInterface320 **dev) {
    return (*dev)->USBDeviceOpen(dev);
}

static int deviceClose(IOUSBDeviceInterface320 **dev) {
    return (*dev)->USBDeviceClose(dev);
}

static int deviceSetConfiguration(IOUSBDeviceInterface320 **dev, uint8_t cfg) {
    return (*dev)->SetConfiguration(dev, cfg);
}

static int deviceGetConfiguration(IOUSBDeviceInterface320 **dev, uint8_t *cfg) {
    return (*dev)->GetConfiguration(dev, cfg);
}

static int deviceControlRequest(IOUSBDeviceInterface320 **dev,
    uint8_t bmReqType, uint8_t bReq, uint16_t wVal, uint16_t wIdx,
    void *data, uint16_t wLen, uint32_t timeoutMs) {
    IOUSBDevRequestTO req;
    req.bmRequestType = bmReqType;
    req.bRequest = bReq;
    req.wValue = wVal;
    req.wIndex = wIdx;
    req.wLength = wLen;
    req.pData = data;
    req.noDataTimeout = timeoutMs;
    req.completionTimeout = timeoutMs;
    return (*dev)->DeviceRequestTO(dev, &req);
}

static int deviceReset(IOUSBDeviceInterface320 **dev) {
    return (*dev)->ResetDevice(dev);
}

static void releaseDeviceInterface(IOUSBDeviceInterface320 **dev) {
    if (dev && *dev) {
        (*dev)->Release(dev);
    }
}

static io_service_t findChildInterface(io_service_t deviceSvc, uint8_t intfNum) {
    io_iterator_t iter = 0;
    kern_return_t kr = IORegistryEntryGetChildIterator(deviceSvc, kIOServicePlane, &iter);
    if (kr != KERN_SUCCESS || !iter) return 0;

    io_service_t found = 0;
    io_service_t child;
    while ((child = IOIteratorNext(iter)) != 0) {
        // Check if this child is an IOUSBInterface
        if (IOObjectConformsTo(child, "IOUSBInterface")) {
            CFNumberRef numRef = IORegistryEntryCreateCFProperty(child,
                CFSTR("bInterfaceNumber"), kCFAllocatorDefault, 0);
            if (numRef) {
                int num = 0;
                CFNumberGetValue(numRef, kCFNumberIntType, &num);
                CFRelease(numRef);
                if ((uint8_t)num == intfNum) {
                    found = child;
                    break;
                }
            }
        }
        IOObjectRelease(child);
    }
    IOObjectRelease(iter);
    return found;
}

static IOUSBInterfaceInterface300** openInterfaceInterface(io_service_t svc) {
    IOCFPlugInInterface **plugin = NULL;
    SInt32 score;
    kern_return_t kr = IOCreatePlugInInterfaceForService(svc,
        kIOUSBInterfaceUserClientTypeID, kIOCFPlugInInterfaceID, &plugin, &score);
    if (kr != KERN_SUCCESS || !plugin) return NULL;

    IOUSBInterfaceInterface300 **iface = NULL;
    HRESULT res = (*plugin)->QueryInterface(plugin,
        CFUUIDGetUUIDBytes(kIOUSBInterfaceInterfaceID300), (LPVOID *)&iface);
    (*plugin)->Release(plugin);
    return (res == 0 && iface) ? iface : NULL;
}

static int interfaceOpen(IOUSBInterfaceInterface300 **intf) {
    return (*intf)->USBInterfaceOpen(intf);
}

static int interfaceClose(IOUSBInterfaceInterface300 **intf) {
    return (*intf)->USBInterfaceClose(intf);
}

static int interfaceSetAltSetting(IOUSBInterfaceInterface300 **intf, uint8_t alt) {
    return (*intf)->SetAlternateInterface(intf, alt);
}

static int interfaceGetNumEndpoints(IOUSBInterfaceInterface300 **intf, uint8_t *num) {
    return (*intf)->GetNumEndpoints(intf, num);
}

static int interfaceGetPipeProperties(IOUSBInterfaceInterface300 **intf,
    uint8_t pipeRef, uint8_t *direction, uint8_t *number,
    uint8_t *transferType, uint16_t *maxPacketSize, uint8_t *interval) {
    return (*intf)->GetPipeProperties(intf, pipeRef, direction, number,
        transferType, maxPacketSize, interval);
}

static int interfaceReadPipeTO(IOUSBInterfaceInterface300 **intf,
    uint8_t pipeRef, void *buf, uint32_t *size, uint32_t timeoutMs) {
    return (*intf)->ReadPipeTO(intf, pipeRef, buf, size, timeoutMs, timeoutMs);
}

static int interfaceWritePipeTO(IOUSBInterfaceInterface300 **intf,
    uint8_t pipeRef, void *buf, uint32_t *size, uint32_t timeoutMs) {
    return (*intf)->WritePipeTO(intf, pipeRef, buf, *size, timeoutMs, timeoutMs);
}

static void releaseInterfaceInterface(IOUSBInterfaceInterface300 **intf) {
    if (intf && *intf) {
        (*intf)->Release(intf);
    }
}

#pragma clang diagnostic pop
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"unsafe"

	"github.com/sirupsen/logrus"
	usb "github.com/kevmo314/go-usb"
)

const (
	kIOReturnSuccess        = 0
	kIOUSBPipeStalled       = -536870897
	kIOUSBTransactionTimeout = -536870899
	kIOReturnNotResponding  = -536870906
	kIOReturnNoDevice       = -536870208
	kIOReturnExclusiveAccess = -536870203
)

// iokitDevice wraps an IOKit IOUSBDeviceInterface320 pointer.
type iokitDevice struct {
	ptr        **C.IOUSBDeviceInterface320
	service    C.io_service_t
	locationID uint32
	opened     bool
}

// iokitInterface wraps an IOKit IOUSBInterfaceInterface300 pointer.
type iokitInterface struct {
	ptr     **C.IOUSBInterfaceInterface300
	service C.io_service_t
	num     uint8
}

// USB/IP speed codes (see linux/usbip.h: enum usb_device_speed mapping).
const (
	usbipSpeedUnknown = 0
	usbipSpeedLow     = 1
	usbipSpeedFull    = 2
	usbipSpeedHigh    = 3
	usbipSpeedSuper   = 5
)

const (
	reqSetConfiguration = 0x09
	reqSetInterface     = 0x0b
)

func inferSpeed(desc usb.DeviceDescriptor) uint32 {
	switch {
	case desc.USBVersion >= 0x0300:
		return usbipSpeedSuper
	case desc.USBVersion >= 0x0200:
		return usbipSpeedHigh
	case desc.USBVersion >= 0x0110:
		return usbipSpeedFull
	default:
		return usbipSpeedLow
	}
}

func busidLocationID(loc uint32) string {
	return fmt.Sprintf("iokit-0x%08x", loc)
}

func deviceDescToInfo(desc usb.DeviceDescriptor, bus uint8, addr uint8, locationID uint32) DeviceInfo {
	info := DeviceInfo{
		BusNum:            uint32(bus),
		DevNum:            uint32(addr),
		Busid:             busidLocationID(locationID),
		Path:              fmt.Sprintf("/sys/devices/lima/usb/%s", busidLocationID(locationID)),
		Speed:             inferSpeed(desc),
		Vendor:            desc.VendorID,
		Product:           desc.ProductID,
		BcdDevice:         desc.DeviceVersion,
		Class:             desc.DeviceClass,
		SubClass:          desc.DeviceSubClass,
		Protocol:          desc.DeviceProtocol,
		NumConfigurations: desc.NumConfigurations,
	}
	return info
}

func List() ([]DeviceInfo, error) {
	devs, err := usb.DeviceList()
	if err != nil {
		return nil, err
	}
	out := make([]DeviceInfo, 0, len(devs))
	for _, d := range devs {
		loc := d.IOKitDevice.LocationID
		info := deviceDescToInfo(d.Descriptor, d.Bus, d.Address, loc)
		info.ConfigurationValue = 1
		out = append(out, info)
	}
	return out, nil
}

func ListNamed() ([]DeviceInfo, error) {
	devs, err := usb.DeviceList()
	if err != nil {
		return nil, err
	}
	out := make([]DeviceInfo, 0, len(devs))
	for _, d := range devs {
		loc := d.IOKitDevice.LocationID
		info := deviceDescToInfo(d.Descriptor, d.Bus, d.Address, loc)
		if d.CachedStrings != nil {
			info.VendorName = d.CachedStrings.Manufacturer
			info.ProductName = d.CachedStrings.Product
		}
		// Open device to get active configuration.
		handle, err := d.Open()
		if err == nil {
			if cfg, err := handle.GetConfiguration(); err == nil {
				info.ConfigurationValue = uint8(cfg)
			}
			_ = handle.Close()
		}
		out = append(out, info)
	}
	return out, nil
}

func allowEntryFor(info DeviceInfo) AllowEntry {
	return AllowEntry{
		VendorID:  fmt.Sprintf("%04x", info.Vendor),
		ProductID: fmt.Sprintf("%04x", info.Product),
		BusAddr:   info.Busid,
	}
}

type allowlistProvider struct {
	instDir string
}

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

type goUSBDevice struct {
	dev  *usb.Device
	info DeviceInfo

	ikdev      *iokitDevice
	mu         sync.Mutex
	cfgValue   uint8
	ifaces     map[int]*iokitInterface
	inPipes    map[uint8]uint8 // endpoint addr → pipeRef
	outPipes   map[uint8]uint8 // endpoint addr → pipeRef
	cfgDesc    *usb.ConfigDescriptor
}

func Open(vendorID, productID uint16, busid string) (Device, error) {
	devs, err := usb.DeviceList()
	if err != nil {
		return nil, err
	}
	if len(devs) == 0 {
		return nil, fmt.Errorf("usb device %04x:%04x not found", vendorID, productID)
	}

	var match *usb.Device
	for _, d := range devs {
		if d.Descriptor.VendorID != vendorID || d.Descriptor.ProductID != productID {
			continue
		}
		if busid != "" {
			loc := d.IOKitDevice.LocationID
			if busidLocationID(loc) != busid {
				continue
			}
		}
		match = d
		break
	}
	if match == nil {
		return nil, fmt.Errorf("usb device %04x:%04x not found", vendorID, productID)
	}

	loc := match.IOKitDevice.LocationID
	svc := C.findDeviceByLocationID(C.uint32_t(loc))
	if svc == 0 {
		return nil, fmt.Errorf("usb device %04x:%04x disappeared", vendorID, productID)
	}

	devIf := C.openDeviceInterface(svc)
	if devIf == nil {
		C.IOObjectRelease(svc)
		return nil, fmt.Errorf("failed to get device interface for %04x:%04x", vendorID, productID)
	}

	opened := false
	if ret := C.deviceOpen(devIf); ret != kIOReturnSuccess {
		if int32(ret) == kIOReturnExclusiveAccess {
			logrus.Debugf("usbip: device %04x:%04x held by kernel driver (open=0x%x), proceeding without USBDeviceOpen",
				vendorID, productID, ret)
		} else {
			C.releaseDeviceInterface(devIf)
			C.IOObjectRelease(svc)
			return nil, fmt.Errorf("failed to open device %04x:%04x: 0x%x", vendorID, productID, ret)
		}
	} else {
		logrus.Debugf("usbip: device %04x:%04x opened normally", vendorID, productID)
		opened = true
	}

	g := &goUSBDevice{
		dev:  match,
		info: deviceDescToInfo(match.Descriptor, match.Bus, match.Address, loc),
		ikdev: &iokitDevice{
			ptr:        devIf,
			service:    svc,
			locationID: loc,
			opened:     opened,
		},
		ifaces:   map[int]*iokitInterface{},
		inPipes:  map[uint8]uint8{},
		outPipes: map[uint8]uint8{},
	}
	return g, nil
}

func (g *goUSBDevice) Info() DeviceInfo {
	return g.info
}

func (g *goUSBDevice) Gone() bool {
	devs, err := usb.DeviceList()
	if err != nil {
		return false
	}
	for _, d := range devs {
		if d.IOKitDevice.LocationID == g.ikdev.locationID &&
			d.Descriptor.VendorID == g.info.Vendor &&
			d.Descriptor.ProductID == g.info.Product {
			return false
		}
	}
	return true
}

func (g *goUSBDevice) Close() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.releaseLocked()
	g.releaseDeviceLocked()
	return nil
}

func (g *goUSBDevice) releaseLocked() {
	for _, intf := range g.ifaces {
		C.interfaceClose(intf.ptr)
		C.releaseInterfaceInterface(intf.ptr)
		if intf.service != 0 {
			C.IOObjectRelease(intf.service)
		}
	}
	g.ifaces = map[int]*iokitInterface{}
	g.inPipes = map[uint8]uint8{}
	g.outPipes = map[uint8]uint8{}
	g.cfgDesc = nil
	g.cfgValue = 0
}

func (g *goUSBDevice) releaseDeviceLocked() {
	if g.ikdev != nil {
		if g.ikdev.ptr != nil {
			if g.ikdev.opened {
				C.deviceClose(g.ikdev.ptr)
			}
			C.releaseDeviceInterface(g.ikdev.ptr)
			g.ikdev.ptr = nil
		}
		if g.ikdev.service != 0 {
			C.IOObjectRelease(g.ikdev.service)
			g.ikdev.service = 0
		}
	}
}

func deviceGoneErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, usb.ErrNoDevice) || errors.Is(err, usb.ErrNotFound) {
		return fmt.Errorf("%w: %w", ErrDeviceGone, err)
	}
	return err
}

func (g *goUSBDevice) Control(ctx context.Context, setup [8]byte, data []byte) (int, error) {
	n, err := g.control(ctx, setup, data)
	return n, deviceGoneErr(err)
}

func (g *goUSBDevice) control(_ context.Context, setup [8]byte, data []byte) (int, error) {
	rType, request, value, index, _ := controlSetup(setup)

	if rType == 0x00 && request == reqSetConfiguration {
		return 0, g.setConfiguration(int(value))
	}
	if rType == 0x01 && request == reqSetInterface {
		return 0, g.setInterface(int(index), int(value))
	}

	var ptr unsafe.Pointer
	if len(data) > 0 {
		ptr = unsafe.Pointer(&data[0])
	}

	ret := C.deviceControlRequest(g.ikdev.ptr,
		C.uint8_t(rType), C.uint8_t(request),
		C.uint16_t(value), C.uint16_t(index),
		ptr, C.uint16_t(len(data)), 5000)

	if ret != kIOReturnSuccess {
		if int32(ret) == kIOReturnNoDevice || int32(ret) == kIOReturnNotResponding {
			return 0, fmt.Errorf("control transfer: %w", ErrDeviceGone)
		}
		return 0, fmt.Errorf("control transfer failed: 0x%x", ret)
	}
	return len(data), nil
}

func (g *goUSBDevice) ensureConfigLocked() error {
	if g.cfgValue != 0 && g.cfgDesc != nil {
		return nil
	}
	num := int(g.info.ConfigurationValue)
	if num == 0 {
		num = 1
	}
	return g.setConfigurationLocked(num)
}

func (g *goUSBDevice) setConfiguration(num int) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.setConfigurationLocked(num)
}

func (g *goUSBDevice) setConfigurationLocked(num int) error {
	if !g.ikdev.opened {
		// Kernel driver holds the device; cannot set configuration.
		// Read whatever configuration is currently active.
		var currentCfg C.uint8_t
		if ret := C.deviceGetConfiguration(g.ikdev.ptr, &currentCfg); ret == kIOReturnSuccess && currentCfg != 0 {
			logrus.Debugf("usbip: using existing config %d (device held by kernel driver)", currentCfg)
			return g.readConfigLocked(int(currentCfg))
		}
		// Fallback: try to read config 1 without setting it.
		logrus.Debugf("usbip: attempting to read config %d without setting (device held by kernel driver)", num)
		return g.readConfigLocked(num)
	}

	g.releaseLocked()
	ret := C.deviceSetConfiguration(g.ikdev.ptr, C.uint8_t(num))
	if ret != kIOReturnSuccess {
		return fmt.Errorf("set configuration %d: 0x%x", num, ret)
	}
	g.cfgValue = uint8(num)
	return g.readConfigLocked(num)
}

func (g *goUSBDevice) readConfigLocked(num int) error {
	// Read config descriptor via control transfer.
	raw := make([]byte, 9)
	rType := uint8(0x80)
	req := uint8(usb.USB_REQ_GET_DESCRIPTOR)
	val := uint16(usb.USB_DT_CONFIG)<<8 | uint16(num-1)
	ptr := unsafe.Pointer(&raw[0])
	ret := C.deviceControlRequest(g.ikdev.ptr,
		C.uint8_t(rType), C.uint8_t(req),
		C.uint16_t(val), C.uint16_t(0),
		ptr, C.uint16_t(9), 5000)
	if ret != kIOReturnSuccess {
		return fmt.Errorf("read config descriptor header: 0x%x", ret)
	}
	totalLen := int(raw[2]) | int(raw[3])<<8
	full := make([]byte, totalLen)
	ptr = unsafe.Pointer(&full[0])
	ret = C.deviceControlRequest(g.ikdev.ptr,
		C.uint8_t(rType), C.uint8_t(req),
		C.uint16_t(val), C.uint16_t(0),
		ptr, C.uint16_t(totalLen), 5000)
	if ret != kIOReturnSuccess {
		return fmt.Errorf("read full config descriptor: 0x%x", ret)
	}

	cfg := &usb.ConfigDescriptor{}
	if err := cfg.Unmarshal(full); err != nil {
		return fmt.Errorf("parse config descriptor: %w", err)
	}
	g.cfgDesc = cfg

	// Populate InterfaceInfo for the USB/IP device descriptor.
	var ifaces []InterfaceInfo
	for _, intf := range cfg.Interfaces {
		if len(intf.AltSettings) == 0 {
			continue
		}
		alt := intf.AltSettings[0]
		ifaces = append(ifaces, InterfaceInfo{
			Class:    alt.InterfaceClass,
			SubClass: alt.InterfaceSubClass,
			Protocol: alt.InterfaceProtocol,
		})
	}
	g.info.Interfaces = ifaces
	g.info.ConfigurationValue = uint8(num)
	g.info.NumConfigurations = g.dev.Descriptor.NumConfigurations
	g.cfgValue = uint8(num)

	return nil
}

func (g *goUSBDevice) setInterface(num, alt int) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if err := g.ensureConfigLocked(); err != nil {
		return err
	}

	if old, ok := g.ifaces[num]; ok {
		C.interfaceClose(old.ptr)
		C.releaseInterfaceInterface(old.ptr)
		if old.service != 0 {
			C.IOObjectRelease(old.service)
		}
		delete(g.ifaces, num)
		g.dropPipesLocked(num)
	}

	childSvc := C.findChildInterface(g.ikdev.service, C.uint8_t(num))
	if childSvc == 0 {
		return fmt.Errorf("set interface %d alt %d: interface not found", num, alt)
	}

	intfIf := C.openInterfaceInterface(childSvc)
	if intfIf == nil {
		C.IOObjectRelease(childSvc)
		return fmt.Errorf("set interface %d alt %d: failed to get interface plugin", num, alt)
	}

	if ret := C.interfaceOpen(intfIf); ret != kIOReturnSuccess {
		C.releaseInterfaceInterface(intfIf)
		C.IOObjectRelease(childSvc)
		return fmt.Errorf("set interface %d alt %d: failed to open: 0x%x", num, alt, ret)
	}

	if alt != 0 {
		if ret := C.interfaceSetAltSetting(intfIf, C.uint8_t(alt)); ret != kIOReturnSuccess {
			C.interfaceClose(intfIf)
			C.releaseInterfaceInterface(intfIf)
			C.IOObjectRelease(childSvc)
			return fmt.Errorf("set interface %d alt %d: 0x%x", num, alt, ret)
		}
	}

	g.ifaces[num] = &iokitInterface{
		ptr:     intfIf,
		service: childSvc,
		num:     uint8(num),
	}
	return nil
}

func (g *goUSBDevice) Transfer(ctx context.Context, ep uint8, in bool, buf []byte) (int, error) {
	n, err := g.transfer(ctx, ep, in, buf)
	return n, deviceGoneErr(err)
}

func (g *goUSBDevice) transfer(_ context.Context, ep uint8, in bool, buf []byte) (int, error) {
	pipeRef, iface, err := g.findPipe(ep, in)
	if err != nil {
		return 0, err
	}
	_ = iface // held via g.ifaces

	size := C.uint32_t(len(buf))
	var ret C.int
	if in {
		ret = C.interfaceReadPipeTO(iface.ptr, C.uint8_t(pipeRef),
			unsafe.Pointer(&buf[0]), &size, 5000)
	} else {
		ret = C.interfaceWritePipeTO(iface.ptr, C.uint8_t(pipeRef),
			unsafe.Pointer(&buf[0]), &size, 5000)
	}

	if ret != kIOReturnSuccess {
		if int32(ret) == kIOReturnNoDevice || int32(ret) == kIOReturnNotResponding {
			return 0, fmt.Errorf("%w: IOKit 0x%x", ErrDeviceGone, ret)
		}
		if int32(ret) == kIOUSBPipeStalled {
			return 0, fmt.Errorf("pipe stalled: 0x%x", ret)
		}
		return 0, fmt.Errorf("transfer failed on ep %02x: 0x%x", ep, ret)
	}
	return int(size), nil
}

// findPipe looks up or discovers the pipe reference for an endpoint.
func (g *goUSBDevice) findPipe(ep uint8, in bool) (uint8, *iokitInterface, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	epAddr := ep & 0x7f
	if in {
		epAddr |= 0x80
	}

	pipeMap := g.outPipes
	if in {
		pipeMap = g.inPipes
	}
	if pipeRef, ok := pipeMap[epAddr]; ok {
		// Find the interface that owns this pipe.
		for _, intf := range g.ifaces {
			if ownsPipe(intf, pipeRef) {
				return pipeRef, intf, nil
			}
		}
	}

	if err := g.ensureConfigLocked(); err != nil {
		return 0, nil, err
	}

	// Find which interface owns this endpoint from the config descriptor.
	wantDir := uint8(0) // OUT
	if in {
		wantDir = 1
	}

	for _, ifaceDesc := range g.cfgDesc.Interfaces {
		for _, alt := range ifaceDesc.AltSettings {
			for _, epDesc := range alt.Endpoints {
				if epDesc.EndpointAddr&0x0f != ep&0x0f {
					continue
				}
				epDir := uint8(0)
				if epDesc.IsInput() {
					epDir = 1
				}
				if epDir != wantDir {
					continue
				}
				if epDesc.TransferType() == usb.TransferTypeIsochronous {
					return 0, nil, fmt.Errorf("endpoint %d is isochronous (unsupported)", ep&0x0f)
				}

				intfNum := int(ifaceDesc.AltSettings[0].InterfaceNumber)

				// Claim the interface if not already claimed.
				intf, ok := g.ifaces[intfNum]
				if !ok {
					childSvc := C.findChildInterface(g.ikdev.service, C.uint8_t(intfNum))
					if childSvc == 0 {
						continue
					}
					intfIf := C.openInterfaceInterface(childSvc)
					if intfIf == nil {
						C.IOObjectRelease(childSvc)
						continue
					}
					if ret := C.interfaceOpen(intfIf); ret != kIOReturnSuccess {
						C.releaseInterfaceInterface(intfIf)
						C.IOObjectRelease(childSvc)
						continue
					}
					intf = &iokitInterface{
						ptr:     intfIf,
						service: childSvc,
						num:     uint8(intfNum),
					}
					g.ifaces[intfNum] = intf
				}

				// Discover pipes.
				var numEP C.uint8_t
				if ret := C.interfaceGetNumEndpoints(intf.ptr, &numEP); ret == kIOReturnSuccess {
					for pipeRef := uint8(1); pipeRef <= uint8(numEP); pipeRef++ {
						var dir, num, xferType C.uint8_t
						var maxPkt C.uint16_t
						var interval C.uint8_t
						if ret := C.interfaceGetPipeProperties(intf.ptr,
							C.uint8_t(pipeRef), &dir, &num,
							&xferType, &maxPkt, &interval); ret == kIOReturnSuccess {
							epAddr := uint8(num)
							if dir == 1 {
								epAddr |= 0x80
								g.inPipes[epAddr] = pipeRef
							} else {
								g.outPipes[epAddr] = pipeRef
							}
						}
					}
				}

				if pipeRef, ok := pipeMap[epAddr]; ok {
					return pipeRef, intf, nil
				}
			}
		}
	}

	return 0, nil, fmt.Errorf("no interface provides endpoint %d (in=%v)", ep&0x0f, in)
}

func ownsPipe(intf *iokitInterface, targetPipeRef uint8) bool {
	var numEP C.uint8_t
	if ret := C.interfaceGetNumEndpoints(intf.ptr, &numEP); ret != kIOReturnSuccess {
		return false
	}
	for pipeRef := uint8(1); pipeRef <= uint8(numEP); pipeRef++ {
		if pipeRef == targetPipeRef {
			return true
		}
	}
	return false
}

func (g *goUSBDevice) dropPipesLocked(intfNum int) {
	if g.cfgDesc == nil {
		return
	}
	for _, intf := range g.cfgDesc.Interfaces {
		if len(intf.AltSettings) == 0 || int(intf.AltSettings[0].InterfaceNumber) != intfNum {
			continue
		}
		for _, alt := range intf.AltSettings {
			for _, epDesc := range alt.Endpoints {
				delete(g.inPipes, epDesc.EndpointAddr)
				delete(g.outPipes, epDesc.EndpointAddr)
			}
		}
	}
}
