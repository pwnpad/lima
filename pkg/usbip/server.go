// SPDX-FileCopyrightText: Copyright The Lima Authors
// SPDX-License-Identifier: Apache-2.0

package usbip

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/sirupsen/logrus"
)

// Linux errno values reported back to the guest in RET_SUBMIT.status.
const (
	errnoEPIPE      = 32 // endpoint stalled
	errnoEOPNOTSUPP = 95 // e.g. isochronous transfers
	errnoECONNRESET = 104
)

// maxTransferLength bounds the buffer a single URB may request, guarding
// against a malicious or buggy guest asking us to allocate unbounded memory.
const maxTransferLength = 16 << 20 // 16 MiB

// Serve handles one USB/IP client connection against the given devices. The
// connection is expected to carry exactly one operation: a device list request
// (answered with every device, then closed) or an import request (matched to a
// device by busid, answered, then followed by the URB exchange that runs until
// the connection closes or ctx is cancelled).
func Serve(ctx context.Context, conn io.ReadWriter, devices []Device) error {
	op, err := readOpHeader(conn)
	if err != nil {
		return fmt.Errorf("reading op header: %w", err)
	}
	switch op.Code {
	case opReqDevlist:
		return writeDevlist(conn, devices)
	case opReqImport:
		return handleImport(ctx, conn, devices)
	default:
		return fmt.Errorf("unsupported op code %#04x", op.Code)
	}
}

func deviceDescWire(info DeviceInfo) usbDeviceDesc {
	var d usbDeviceDesc
	toCString(d.Path[:], info.Path)
	toCString(d.Busid[:], info.Busid)
	d.Busnum = info.BusNum
	d.Devnum = info.DevNum
	d.Speed = info.Speed
	d.IDVendor = info.Vendor
	d.IDProduct = info.Product
	d.BcdDevice = info.BcdDevice
	d.BDeviceClass = info.Class
	d.BDeviceSubClass = info.SubClass
	d.BDeviceProtocol = info.Protocol
	d.BConfigurationValue = info.ConfigurationValue
	d.BNumConfigurations = info.NumConfigurations
	d.BNumInterfaces = uint8(len(info.Interfaces))
	return d
}

func writeDevlist(conn io.Writer, devices []Device) error {
	if err := writeOpHeader(conn, opRepDevlist, 0); err != nil {
		return err
	}
	if err := binary.Write(conn, binary.BigEndian, uint32(len(devices))); err != nil {
		return err
	}
	for _, dev := range devices {
		info := dev.Info()
		if err := binary.Write(conn, binary.BigEndian, deviceDescWire(info)); err != nil {
			return err
		}
		for _, iface := range info.Interfaces {
			if err := binary.Write(conn, binary.BigEndian, usbInterfaceDesc{
				BInterfaceClass:    iface.Class,
				BInterfaceSubClass: iface.SubClass,
				BInterfaceProtocol: iface.Protocol,
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

func handleImport(ctx context.Context, conn io.ReadWriter, devices []Device) error {
	var busid [32]byte
	if _, err := io.ReadFull(conn, busid[:]); err != nil {
		return fmt.Errorf("reading import busid: %w", err)
	}
	requested := string(busid[:clen(busid[:])])
	var dev Device
	for _, d := range devices {
		if d.Info().Busid == requested {
			dev = d
			break
		}
	}
	if dev == nil {
		logrus.Warnf("usbip: import for unknown busid %q", requested)
		return writeOpHeader(conn, opRepImport, 1)
	}
	if err := writeOpHeader(conn, opRepImport, 0); err != nil {
		return err
	}
	if err := binary.Write(conn, binary.BigEndian, deviceDescWire(dev.Info())); err != nil {
		return err
	}
	return urbLoop(ctx, conn, dev)
}

type inflight struct {
	cancel   context.CancelFunc
	unlinked bool
}

// urbLoop reads URB commands until the connection closes or ctx is cancelled.
// CMD_SUBMIT requests are dispatched to per-URB goroutines so a blocking IN
// transfer (e.g. an interrupt endpoint) does not stall other endpoints; the
// inbound stream itself is always read synchronously to stay frame-aligned.
func urbLoop(ctx context.Context, conn io.ReadWriter, dev Device) error {
	var writeMu sync.Mutex
	var stateMu sync.Mutex
	pending := map[uint32]*inflight{}
	var wg sync.WaitGroup
	defer wg.Wait()

	for {
		h, err := readURBHeader(conn)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("reading urb header: %w", err)
		}

		switch h.Command {
		case cmdSubmit:
			var outData []byte
			if int(h.Direction) == dirOut && h.TransferBufferLength > 0 {
				if h.TransferBufferLength > maxTransferLength {
					return fmt.Errorf("urb transfer length %d exceeds limit", h.TransferBufferLength)
				}
				outData = make([]byte, h.TransferBufferLength)
				if _, err := io.ReadFull(conn, outData); err != nil {
					return fmt.Errorf("reading urb out data: %w", err)
				}
			} else if h.TransferBufferLength > maxTransferLength {
				return fmt.Errorf("urb transfer length %d exceeds limit", h.TransferBufferLength)
			}

			urbCtx, cancel := context.WithCancel(ctx)
			entry := &inflight{cancel: cancel}
			stateMu.Lock()
			pending[h.Seqnum] = entry
			stateMu.Unlock()

			wg.Add(1)
			go func() {
				defer wg.Done()
				defer cancel()
				status, actual, inData := doSubmit(urbCtx, dev, h, outData)

				stateMu.Lock()
				unlinked := entry.unlinked
				delete(pending, h.Seqnum)
				stateMu.Unlock()
				if unlinked {
					// The guest already cancelled this URB via CMD_UNLINK and
					// received a RET_UNLINK; do not also send a RET_SUBMIT.
					return
				}

				writeMu.Lock()
				defer writeMu.Unlock()
				if err := writeRetSubmit(conn, h, status, actual, inData); err != nil {
					logrus.WithError(err).Debug("usbip: writing ret_submit failed")
				}
			}()

		case cmdUnlink:
			victim := decodeUnlinkSeqnum(h)
			stateMu.Lock()
			if entry, ok := pending[victim]; ok {
				entry.unlinked = true
				entry.cancel()
			}
			stateMu.Unlock()
			writeMu.Lock()
			err := writeRetUnlink(conn, h, 0)
			writeMu.Unlock()
			if err != nil {
				return fmt.Errorf("writing ret_unlink: %w", err)
			}

		default:
			return fmt.Errorf("unsupported urb command %#08x", h.Command)
		}
	}
}

// doSubmit performs a single URB and returns the errno-style status, the number
// of bytes transferred, and (for IN transfers) the data read from the device.
func doSubmit(ctx context.Context, dev Device, h urbHeader, outData []byte) (status, actual int32, inData []byte) {
	if h.NumberOfPackets > 0 {
		// Isochronous transfer; unsupported.
		return -errnoEOPNOTSUPP, 0, nil
	}

	in := int(h.Direction) == dirIn
	var buf []byte
	if in {
		buf = make([]byte, h.TransferBufferLength)
	} else {
		buf = outData
	}

	var (
		n   int
		err error
	)
	if h.Ep == 0 {
		// Control transfer on endpoint 0. The SETUP packet's own direction bit
		// is authoritative for gousb.
		cbuf := buf
		if setupIsIn(h.Setup) {
			if len(cbuf) == 0 {
				_, _, _, _, wlen := controlSetup(h.Setup)
				cbuf = make([]byte, wlen)
			}
		}
		n, err = dev.Control(ctx, h.Setup, cbuf)
		if in {
			inData = cbuf
		}
	} else {
		n, err = dev.Transfer(ctx, uint8(h.Ep), in, buf)
		if in {
			inData = buf
		}
	}

	if err != nil {
		if ctx.Err() != nil {
			return -errnoECONNRESET, 0, nil
		}
		logrus.WithError(err).Debugf("usbip: transfer failed on ep %d", h.Ep)
		return -errnoEPIPE, 0, nil
	}
	return 0, int32(n), inData
}

// clen returns the length of the NUL-terminated prefix of b.
func clen(b []byte) int {
	for i, c := range b {
		if c == 0 {
			return i
		}
	}
	return len(b)
}
