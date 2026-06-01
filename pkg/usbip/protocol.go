// SPDX-FileCopyrightText: Copyright The Lima Authors
// SPDX-License-Identifier: Apache-2.0

// Package usbip implements the server ("stub") side of the USB/IP protocol,
// backed by libusb (via gousb). It lets a Linux guest import a physical USB
// device attached to the macOS host onto its vhci-hcd virtual host controller,
// so the device appears as if natively plugged into the guest.
//
// Only control, bulk, and interrupt transfers are supported. Isochronous
// transfers are rejected. The wire format follows the Linux kernel USB/IP
// protocol (Documentation/usb/usbip_protocol.rst); all multi-byte integers in
// the headers are big-endian (network byte order).
package usbip

import (
	"encoding/binary"
	"fmt"
	"io"
)

// Protocol version, BCD 1.1.1.
const protocolVersion = 0x0111

// Operation codes used during the connection (pre-import) phase.
const (
	opReqDevlist = 0x8005
	opRepDevlist = 0x0005
	opReqImport  = 0x8003
	opRepImport  = 0x0003
)

// Command codes used during the URB phase (after a successful import).
const (
	cmdSubmit = 0x00000001
	retSubmit = 0x00000003
	cmdUnlink = 0x00000002
	retUnlink = 0x00000004
)

// URB transfer direction, from the host's perspective.
const (
	dirOut = 0
	dirIn  = 1
)

// opHeader is the 8-byte header that prefixes every message in the
// connection phase: version, operation code, and a status field.
type opHeader struct {
	Version uint16
	Code    uint16
	Status  uint32
}

// usbDeviceDesc is the on-wire description of a USB device, used in the
// OP_REP_DEVLIST and OP_REP_IMPORT replies. Total size is 312 bytes.
type usbDeviceDesc struct {
	Path                [256]byte
	Busid               [32]byte
	Busnum              uint32
	Devnum              uint32
	Speed               uint32
	IDVendor            uint16
	IDProduct           uint16
	BcdDevice           uint16
	BDeviceClass        uint8
	BDeviceSubClass     uint8
	BDeviceProtocol     uint8
	BConfigurationValue uint8
	BNumConfigurations  uint8
	BNumInterfaces      uint8
}

// usbInterfaceDesc is the on-wire description of a single USB interface, only
// emitted as part of OP_REP_DEVLIST.
type usbInterfaceDesc struct {
	BInterfaceClass    uint8
	BInterfaceSubClass uint8
	BInterfaceProtocol uint8
	_                  uint8 // padding
}

// urbHeader is the fixed 48-byte header of a USBIP_CMD_SUBMIT message. The
// USBIP_RET_SUBMIT and *_UNLINK messages share the same 48-byte envelope but
// reinterpret the trailing 28 bytes; this struct covers SUBMIT, which is the
// only inbound command that carries those fields.
type urbHeader struct {
	Command              uint32
	Seqnum               uint32
	Devid                uint32
	Direction            uint32
	Ep                   uint32
	TransferFlags        uint32
	TransferBufferLength int32
	StartFrame           int32
	NumberOfPackets      int32
	Interval             int32
	Setup                [8]byte
}

// urbHeaderSize is the wire size of every URB-phase header (both directions).
const urbHeaderSize = 48

func toCString(dst []byte, s string) {
	n := copy(dst, s)
	if n < len(dst) {
		dst[n] = 0
	}
}

func readOpHeader(r io.Reader) (opHeader, error) {
	var h opHeader
	err := binary.Read(r, binary.BigEndian, &h)
	return h, err
}

func writeOpHeader(w io.Writer, code uint16, status uint32) error {
	return binary.Write(w, binary.BigEndian, opHeader{
		Version: protocolVersion,
		Code:    code,
		Status:  status,
	})
}

// readURBHeader reads the fixed 48-byte URB envelope and decodes it as a
// SUBMIT header. The caller must inspect Command to decide how to interpret
// unlink-specific fields (the Seqnum-to-unlink is carried in TransferFlags'
// position for UNLINK; see decodeUnlinkSeqnum).
func readURBHeader(r io.Reader) (urbHeader, error) {
	var buf [urbHeaderSize]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return urbHeader{}, err
	}
	var h urbHeader
	h.Command = binary.BigEndian.Uint32(buf[0:])
	h.Seqnum = binary.BigEndian.Uint32(buf[4:])
	h.Devid = binary.BigEndian.Uint32(buf[8:])
	h.Direction = binary.BigEndian.Uint32(buf[12:])
	h.Ep = binary.BigEndian.Uint32(buf[16:])
	h.TransferFlags = binary.BigEndian.Uint32(buf[20:])
	h.TransferBufferLength = int32(binary.BigEndian.Uint32(buf[24:]))
	h.StartFrame = int32(binary.BigEndian.Uint32(buf[28:]))
	h.NumberOfPackets = int32(binary.BigEndian.Uint32(buf[32:]))
	h.Interval = int32(binary.BigEndian.Uint32(buf[36:]))
	copy(h.Setup[:], buf[40:48])
	return h, nil
}

// decodeUnlinkSeqnum extracts the seqnum-to-cancel from a USBIP_CMD_UNLINK
// envelope. In UNLINK the field at offset 20 holds the victim seqnum, which
// readURBHeader parses into TransferFlags.
func decodeUnlinkSeqnum(h urbHeader) uint32 {
	return h.TransferFlags
}

// writeRetSubmit writes a USBIP_RET_SUBMIT header followed by the inbound
// transfer buffer (for IN transfers). status uses Linux errno conventions
// (0 == success, negative == -errno).
func writeRetSubmit(w io.Writer, h urbHeader, status, actualLength int32, data []byte) error {
	var buf [urbHeaderSize]byte
	binary.BigEndian.PutUint32(buf[0:], retSubmit)
	binary.BigEndian.PutUint32(buf[4:], h.Seqnum)
	binary.BigEndian.PutUint32(buf[8:], h.Devid)
	binary.BigEndian.PutUint32(buf[12:], h.Direction)
	binary.BigEndian.PutUint32(buf[16:], h.Ep)
	binary.BigEndian.PutUint32(buf[20:], uint32(status))
	binary.BigEndian.PutUint32(buf[24:], uint32(actualLength))
	// start_frame, number_of_packets, error_count, and the 8-byte setup
	// padding all remain zero for non-isochronous transfers.
	if _, err := w.Write(buf[:]); err != nil {
		return err
	}
	if int(h.Direction) == dirIn && actualLength > 0 {
		if _, err := w.Write(data[:actualLength]); err != nil {
			return err
		}
	}
	return nil
}

// writeRetUnlink writes a USBIP_RET_UNLINK header with the given errno status.
func writeRetUnlink(w io.Writer, h urbHeader, status int32) error {
	var buf [urbHeaderSize]byte
	binary.BigEndian.PutUint32(buf[0:], retUnlink)
	binary.BigEndian.PutUint32(buf[4:], h.Seqnum)
	binary.BigEndian.PutUint32(buf[8:], h.Devid)
	binary.BigEndian.PutUint32(buf[12:], h.Direction)
	binary.BigEndian.PutUint32(buf[16:], h.Ep)
	binary.BigEndian.PutUint32(buf[20:], uint32(status))
	_, err := w.Write(buf[:])
	return err
}

// controlSetup decodes the 8-byte SETUP packet of a control transfer into the
// argument shape expected by libusb/gousb. wValue and wIndex are little-endian
// in the SETUP packet.
func controlSetup(setup [8]byte) (rType, request uint8, value, index, length uint16) {
	rType = setup[0]
	request = setup[1]
	value = binary.LittleEndian.Uint16(setup[2:])
	index = binary.LittleEndian.Uint16(setup[4:])
	length = binary.LittleEndian.Uint16(setup[6:])
	return
}

func setupIsIn(setup [8]byte) bool {
	return setup[0]&0x80 != 0
}

func busidString(bus, addr int) string {
	return fmt.Sprintf("%d-%d", bus, addr)
}
