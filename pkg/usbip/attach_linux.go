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

// vsock endpoint of the host USB/IP server.
const (
	hostCID   = unix.VMADDR_CID_HOST
	usbipPort = 2223
)

const (
	vhciStatusPath = "/sys/devices/platform/vhci_hcd.0/status"
	vhciAttachPath = "/sys/devices/platform/vhci_hcd.0/attach"
	vhciDetachPath = "/sys/devices/platform/vhci_hcd.0/detach"
	usbDevicesPath = "/sys/bus/usb/devices"
)

const (
	vdevStNull  = 4
	vdevStUsed  = 6
	vdevStError = 7
)

const usbipSpeedSuper = 5

const portStateDir = "/run/lima-guestagent"

var portStatePath = filepath.Join(portStateDir, "usbip-ports.json")

type AttachedDevice struct {
	VIDPID string
	Busid  string
}

func readPortState() map[string]string {
	m := map[string]string{}
	b, err := os.ReadFile(portStatePath)
	if err != nil {
		return m
	}
	_ = json.Unmarshal(b, &m)
	return m
}

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

func recordPort(port int, busid string) {
	m := readPortState()
	m[strconv.Itoa(port)] = busid
	_ = writePortState(m)
}

func forgetPort(port int) {
	m := readPortState()
	if _, ok := m[strconv.Itoa(port)]; !ok {
		return
	}
	delete(m, strconv.Itoa(port))
	_ = writePortState(m)
}

// fdConn adapts a raw blocking socket fd to io.ReadWriter without letting the
// Go runtime take ownership (the kernel receives ownership on attach).
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

func AttachBusid(busid string) error {
	// Reclaim errored ports from earlier physical unplugs.
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
	return nil
}

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
		for j := uint8(0); j < desc.BNumInterfaces; j++ {
			var iface usbInterfaceDesc
			if err := binary.Read(rw, binary.BigEndian, &iface); err != nil {
				return nil, fmt.Errorf("reading interface descriptor: %w", err)
			}
		}
	}
	return busids, nil
}

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
