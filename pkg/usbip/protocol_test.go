// SPDX-FileCopyrightText: Copyright The Lima Authors
// SPDX-License-Identifier: Apache-2.0

package usbip

import (
	"bytes"
	"encoding/binary"
	"testing"

	"gotest.tools/v3/assert"
)

func TestWireSizes(t *testing.T) {
	assert.Equal(t, binary.Size(usbDeviceDesc{}), 312)
	assert.Equal(t, binary.Size(usbInterfaceDesc{}), 4)
	assert.Equal(t, binary.Size(opHeader{}), 8)
	assert.Equal(t, binary.Size(urbHeader{}), urbHeaderSize)
}

func TestReadURBHeaderRoundTrip(t *testing.T) {
	want := urbHeader{
		Command:              cmdSubmit,
		Seqnum:               42,
		Devid:                (3 << 16) | 7,
		Direction:            dirIn,
		Ep:                   0x81 & 0x0f,
		TransferFlags:        0,
		TransferBufferLength: 64,
		Interval:             10,
		Setup:                [8]byte{0x80, 0x06, 0x00, 0x01, 0x00, 0x00, 0x12, 0x00},
	}

	var buf [urbHeaderSize]byte
	binary.BigEndian.PutUint32(buf[0:], want.Command)
	binary.BigEndian.PutUint32(buf[4:], want.Seqnum)
	binary.BigEndian.PutUint32(buf[8:], want.Devid)
	binary.BigEndian.PutUint32(buf[12:], want.Direction)
	binary.BigEndian.PutUint32(buf[16:], want.Ep)
	binary.BigEndian.PutUint32(buf[20:], want.TransferFlags)
	binary.BigEndian.PutUint32(buf[24:], uint32(want.TransferBufferLength))
	binary.BigEndian.PutUint32(buf[36:], uint32(want.Interval))
	copy(buf[40:], want.Setup[:])

	got, err := readURBHeader(bytes.NewReader(buf[:]))
	assert.NilError(t, err)
	assert.DeepEqual(t, got, want)
}

func TestControlSetupDecode(t *testing.T) {
	// GET_DESCRIPTOR(device): bmRequestType=0x80, bRequest=0x06,
	// wValue=0x0100, wIndex=0x0000, wLength=0x0012.
	setup := [8]byte{0x80, 0x06, 0x00, 0x01, 0x00, 0x00, 0x12, 0x00}
	rType, request, value, index, length := controlSetup(setup)
	assert.Equal(t, rType, uint8(0x80))
	assert.Equal(t, request, uint8(0x06))
	assert.Equal(t, value, uint16(0x0100))
	assert.Equal(t, index, uint16(0x0000))
	assert.Equal(t, length, uint16(0x0012))
	assert.Equal(t, setupIsIn(setup), true)
}

func TestWriteRetSubmitIn(t *testing.T) {
	h := urbHeader{Seqnum: 9, Devid: 1, Direction: dirIn, Ep: 0}
	payload := []byte{0xde, 0xad, 0xbe, 0xef}
	var out bytes.Buffer
	assert.NilError(t, writeRetSubmit(&out, h, 0, int32(len(payload)), payload))

	b := out.Bytes()
	assert.Equal(t, len(b), urbHeaderSize+len(payload))
	assert.Equal(t, binary.BigEndian.Uint32(b[0:]), uint32(retSubmit))
	assert.Equal(t, binary.BigEndian.Uint32(b[4:]), uint32(9))
	assert.Equal(t, int32(binary.BigEndian.Uint32(b[24:])), int32(len(payload)))
	assert.DeepEqual(t, b[urbHeaderSize:], payload)
}

func TestDevlistWrite(t *testing.T) {
	info := DeviceInfo{
		Busid:             "20-3",
		Vendor:            0x0bda,
		Product:           0x8812,
		NumConfigurations: 1,
		Interfaces:        []InterfaceInfo{{Class: 0xff}},
	}
	var out bytes.Buffer
	assert.NilError(t, writeDevlist(&out, []DeviceInfo{info}))

	b := out.Bytes()
	// opHeader(8) + count(4) + usbDeviceDesc(312) + usbInterfaceDesc(4)
	assert.Equal(t, len(b), 8+4+312+4)
	assert.Equal(t, binary.BigEndian.Uint16(b[2:]), uint16(opRepDevlist))
	assert.Equal(t, binary.BigEndian.Uint32(b[8:]), uint32(1))
}
