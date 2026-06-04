// SPDX-FileCopyrightText: Copyright The Lima Authors
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package usbip

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// vsock endpoint of the host USB/IP server (see pkg/driver/vz/usb_darwin.go).
const (
	hostCID   = unix.VMADDR_CID_HOST // 2
	usbipPort = 2223
)

// vhci-hcd sysfs interface. The kernel's virtual USB host controller is driven
// entirely through these files: writing "<port> <sockfd> <devid> <speed>" to
// attach hands it a connected socket it then speaks USB/IP on; writing a port
// number to detach tears the import down; status enumerates the ports.
const (
	vhciStatusPath = "/sys/devices/platform/vhci_hcd.0/status"
	vhciAttachPath = "/sys/devices/platform/vhci_hcd.0/attach"
	vhciDetachPath = "/sys/devices/platform/vhci_hcd.0/detach"
	usbDevicesPath = "/sys/bus/usb/devices"
)

// vhci port status values (kernel enum usbip_device_status). A port is free
// when VDEV_ST_NULL and carries an imported device when VDEV_ST_USED.
const (
	vdevStNull  = 4
	vdevStUsed  = 6
	vdevStError = 7
)

// usbipSpeedSuper mirrors the super-speed code; SuperSpeed devices must attach
// to the controller's "ss" hub, everything else to "hs".
const usbipSpeedSuper = 5

// portStateDir holds a small map from vhci port number to the host busid that
// was imported onto it. vhci's own status only exposes the guest-side local
// busid, so this is the only place the original host busid survives — it lets
// `usbip port` report which physical host device backs each import, which in
// turn lets limactl tell identical (same vid:pid) devices apart. Kept on tmpfs:
// imports do not survive a reboot, so neither should this state.
const portStateDir = "/run/lima-guestagent"

var portStatePath = filepath.Join(portStateDir, "usbip-ports.json")

// AttachedDevice is one device currently imported onto the guest's vhci-hcd
// controller: its vendor:product and, when known, the host busid it came from.
type AttachedDevice struct {
	VIDPID string
	Busid  string
}

// readPortState loads the port -> host busid map. A missing file yields an empty
// map (no imports recorded yet).
func readPortState() map[string]string {
	m := map[string]string{}
	b, err := os.ReadFile(portStatePath)
	if err != nil {
		return m
	}
	_ = json.Unmarshal(b, &m)
	return m
}

// writePortState atomically persists the port -> host busid map.
func writePortState(m map[string]string) error {
	if err := os.MkdirAll(portStateDir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(portStateDir, "usbip-ports.*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, portStatePath)
}

// recordPort remembers that vhci port carries the given host busid.
func recordPort(port int, busid string) {
	m := readPortState()
	m[strconv.Itoa(port)] = busid
	_ = writePortState(m)
}

// forgetPort drops the mapping for a vhci port after it is detached.
func forgetPort(port int) {
	m := readPortState()
	if _, ok := m[strconv.Itoa(port)]; !ok {
		return
	}
	delete(m, strconv.Itoa(port))
	_ = writePortState(m)
}

// fdConn adapts a raw blocking socket fd to io.ReadWriter so the big-endian
// protocol helpers in protocol.go can be reused for the import handshake. We
// deliberately keep the fd a plain syscall fd (not os.File/net.Conn) so the Go
// runtime netpoller never takes ownership of a descriptor we hand to the kernel.
type fdConn struct{ fd int }

func (c fdConn) Write(p []byte) (int, error) {
	total := 0
	for total < len(p) {
		n, err := unix.Write(c.fd, p[total:])
		if n > 0 {
			total += n
		}
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return total, err
		}
		if n == 0 {
			return total, io.ErrShortWrite
		}
	}
	return total, nil
}

func (c fdConn) Read(p []byte) (int, error) {
	for {
		n, err := unix.Read(c.fd, p)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return n, err
		}
		if n == 0 {
			return 0, io.EOF
		}
		return n, nil
	}
}

// dialHost opens a blocking AF_VSOCK connection to the host USB/IP server and
// returns the raw fd.
func dialHost() (int, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
	if err != nil {
		return -1, fmt.Errorf("creating vsock socket: %w", err)
	}
	if err := unix.Connect(fd, &unix.SockaddrVM{CID: hostCID, Port: usbipPort}); err != nil {
		_ = unix.Close(fd)
		return -1, fmt.Errorf("connecting to host vsock %d:%d: %w", hostCID, usbipPort, err)
	}
	return fd, nil
}

// AttachBusid imports the host device identified by busid onto the guest's
// vhci-hcd controller over vsock. On success the kernel takes ownership of the
// socket; the fd is intentionally left open (closed when the process exits,
// after the kernel has its own reference).
func AttachBusid(busid string) error {
	// Reclaim any ports left in VDEV_ST_ERROR by an earlier physical unplug, so a
	// dead import does not consume a vhci slot and block this attach.
	if _, err := DetachErrorPorts(); err != nil {
		fmt.Fprintf(os.Stderr, "usbip: reclaiming errored ports failed: %v\n", err)
	}

	fd, err := dialHost()
	if err != nil {
		return err
	}
	rw := fdConn{fd}

	if err := writeOpHeader(rw, opReqImport, 0); err != nil {
		_ = unix.Close(fd)
		return fmt.Errorf("sending import request: %w", err)
	}
	var b [32]byte
	toCString(b[:], busid)
	if _, err := rw.Write(b[:]); err != nil {
		_ = unix.Close(fd)
		return fmt.Errorf("sending import busid: %w", err)
	}
	h, err := readOpHeader(rw)
	if err != nil {
		_ = unix.Close(fd)
		return fmt.Errorf("reading import reply: %w", err)
	}
	if h.Status != 0 {
		_ = unix.Close(fd)
		return fmt.Errorf("host refused import of %s (status %d)", busid, h.Status)
	}
	var desc usbDeviceDesc
	if err := binary.Read(rw, binary.BigEndian, &desc); err != nil {
		_ = unix.Close(fd)
		return fmt.Errorf("reading device descriptor: %w", err)
	}

	devid := desc.Busnum<<16 | desc.Devnum
	port, err := freePort(desc.Speed)
	if err != nil {
		_ = unix.Close(fd)
		return err
	}
	line := fmt.Sprintf("%d %d %d %d", port, fd, devid, desc.Speed)
	if err := writeSysfs(vhciAttachPath, line); err != nil {
		_ = unix.Close(fd)
		return fmt.Errorf("attaching %s to vhci port %d: %w", busid, port, err)
	}
	recordPort(port, busid)
	// Do not close fd: the kernel now drives USB/IP on it.
	return nil
}

// AttachAll imports every device the host currently advertises (best effort).
func AttachAll() error {
	busids, err := hostDevlist()
	if err != nil {
		return err
	}
	for _, b := range busids {
		if err := AttachBusid(b); err != nil {
			fmt.Fprintf(os.Stderr, "usbip: attaching %s failed: %v\n", b, err)
		}
	}
	return nil
}

// DetachVIDPID detaches the imported device matching "vid:pid" by resolving its
// vhci port from the controller status.
func DetachVIDPID(vidpid string) error {
	want := strings.ToLower(vidpid)
	ports, err := attachedPorts()
	if err != nil {
		return err
	}
	for _, p := range ports {
		if p.vidpid == want {
			if err := writeSysfs(vhciDetachPath, strconv.Itoa(p.port)); err != nil {
				return err
			}
			forgetPort(p.port)
			return nil
		}
	}
	return fmt.Errorf("device %s is not attached", vidpid)
}

// AttachedDevices returns every device currently imported onto the guest's
// vhci-hcd controller, each as its "vid:pid" plus the host busid it was imported
// from (empty if the import predates port-state tracking).
func AttachedDevices() ([]AttachedDevice, error) {
	ports, err := attachedPorts()
	if err != nil {
		return nil, err
	}
	state := readPortState()
	out := make([]AttachedDevice, 0, len(ports))
	for _, p := range ports {
		out = append(out, AttachedDevice{VIDPID: p.vidpid, Busid: state[strconv.Itoa(p.port)]})
	}
	return out, nil
}

// DetachErrorPorts detaches every vhci port the kernel has marked
// VDEV_ST_ERROR. A port enters that state when the host server drops its USB/IP
// connection on a physical unplug, so reclaiming these ports frees the slots a
// dead import would otherwise hold. Returns the number detached.
func DetachErrorPorts() (int, error) {
	lines, err := statusLines()
	if err != nil {
		return 0, err
	}
	detached := 0
	for _, f := range lines {
		if len(f) < 3 {
			continue
		}
		if sta, err := strconv.Atoi(f[2]); err != nil || sta != vdevStError {
			continue
		}
		port, err := strconv.Atoi(f[1])
		if err != nil {
			continue
		}
		if err := writeSysfs(vhciDetachPath, strconv.Itoa(port)); err != nil {
			return detached, fmt.Errorf("detaching errored vhci port %d: %w", port, err)
		}
		forgetPort(port)
		detached++
	}
	return detached, nil
}

// hostDevlist performs an OP_REQ_DEVLIST exchange and returns the advertised
// host busids.
func hostDevlist() ([]string, error) {
	fd, err := dialHost()
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd)
	rw := fdConn{fd}

	if err := writeOpHeader(rw, opReqDevlist, 0); err != nil {
		return nil, fmt.Errorf("sending devlist request: %w", err)
	}
	h, err := readOpHeader(rw)
	if err != nil {
		return nil, fmt.Errorf("reading devlist reply: %w", err)
	}
	if h.Status != 0 {
		return nil, fmt.Errorf("host devlist returned status %d", h.Status)
	}
	var count uint32
	if err := binary.Read(rw, binary.BigEndian, &count); err != nil {
		return nil, fmt.Errorf("reading devlist count: %w", err)
	}
	var busids []string
	for i := uint32(0); i < count; i++ {
		var desc usbDeviceDesc
		if err := binary.Read(rw, binary.BigEndian, &desc); err != nil {
			return nil, fmt.Errorf("reading devlist entry %d: %w", i, err)
		}
		busids = append(busids, string(desc.Busid[:clen(desc.Busid[:])]))
		// Consume the per-interface descriptors that follow each device.
		for j := uint8(0); j < desc.BNumInterfaces; j++ {
			var iface usbInterfaceDesc
			if err := binary.Read(rw, binary.BigEndian, &iface); err != nil {
				return nil, fmt.Errorf("reading interface descriptor: %w", err)
			}
		}
	}
	return busids, nil
}

// freePort returns the first unused vhci port on the hub matching the device
// speed (super-speed devices need the "ss" hub, everything else "hs").
func freePort(speed uint32) (int, error) {
	wantHub := "hs"
	if speed == usbipSpeedSuper {
		wantHub = "ss"
	}
	lines, err := statusLines()
	if err != nil {
		return 0, err
	}
	for _, f := range lines {
		if len(f) < 3 || f[0] != wantHub {
			continue
		}
		if sta, err := strconv.Atoi(f[2]); err != nil || sta != vdevStNull {
			continue
		}
		port, err := strconv.Atoi(f[1])
		if err != nil {
			continue
		}
		return port, nil
	}
	return 0, fmt.Errorf("no free vhci %s port available", wantHub)
}

type attachedPort struct {
	port   int
	vidpid string
}

// attachedPorts parses the vhci status for ports in use and resolves each to the
// imported device's "vid:pid" via its guest-side sysfs entry (the status line's
// trailing local busid, e.g. "3-1").
func attachedPorts() ([]attachedPort, error) {
	lines, err := statusLines()
	if err != nil {
		return nil, err
	}
	var out []attachedPort
	for _, f := range lines {
		if len(f) < 4 {
			continue
		}
		if sta, err := strconv.Atoi(f[2]); err != nil || sta != vdevStUsed {
			continue
		}
		port, err := strconv.Atoi(f[1])
		if err != nil {
			continue
		}
		localBusid := f[len(f)-1]
		vid, pid, err := readVIDPID(localBusid)
		if err != nil {
			continue
		}
		out = append(out, attachedPort{port: port, vidpid: vid + ":" + pid})
	}
	return out, nil
}

// statusLines reads the vhci status file and returns each data line split into
// whitespace-separated fields (the header line is left in but never matches a
// hub/status filter).
func statusLines() ([][]string, error) {
	data, err := os.ReadFile(vhciStatusPath)
	if err != nil {
		return nil, fmt.Errorf("reading vhci status (is vhci_hcd loaded?): %w", err)
	}
	var out [][]string
	for _, line := range strings.Split(string(data), "\n") {
		if f := strings.Fields(line); len(f) > 0 {
			out = append(out, f)
		}
	}
	return out, nil
}

func readVIDPID(localBusid string) (vid, pid string, err error) {
	dir := filepath.Join(usbDevicesPath, localBusid)
	v, err := os.ReadFile(filepath.Join(dir, "idVendor"))
	if err != nil {
		return "", "", err
	}
	p, err := os.ReadFile(filepath.Join(dir, "idProduct"))
	if err != nil {
		return "", "", err
	}
	return strings.ToLower(strings.TrimSpace(string(v))), strings.ToLower(strings.TrimSpace(string(p))), nil
}

func writeSysfs(path, data string) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	_, werr := f.WriteString(data)
	cerr := f.Close()
	if werr != nil {
		return werr
	}
	return cerr
}
