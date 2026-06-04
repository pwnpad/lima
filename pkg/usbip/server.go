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
	"time"

	"github.com/sirupsen/logrus"
)

// Linux errno values reported back to the guest in RET_SUBMIT.status.
const (
	errnoENODEV     = 19 // device disconnected (physically unplugged)
	errnoEPIPE      = 32 // endpoint stalled
	errnoEOPNOTSUPP = 95 // e.g. isochronous transfers
	errnoECONNRESET = 104
)

// maxTransferLength bounds the buffer a single URB may request, guarding
// against a malicious or buggy guest asking us to allocate unbounded memory.
const maxTransferLength = 16 << 20 // 16 MiB

// Serve handles one USB/IP client connection. The connection carries exactly
// one operation: a device list request (answered with every currently
// exportable device, then closed) or an import request (matched to a device by
// busid, opened on demand, then followed by the URB exchange that runs until the
// connection closes or ctx is cancelled). The provider is queried per call so
// allowlist changes apply without restarting the server.
func Serve(ctx context.Context, conn io.ReadWriter, p Provider) error {
	op, err := readOpHeader(conn)
	if err != nil {
		return fmt.Errorf("reading op header: %w", err)
	}
	switch op.Code {
	case opReqDevlist:
		infos, err := p.Devices()
		if err != nil {
			return fmt.Errorf("listing devices: %w", err)
		}
		return writeDevlist(conn, infos)
	case opReqImport:
		return handleImport(ctx, conn, p)
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

func writeDevlist(conn io.Writer, infos []DeviceInfo) error {
	if err := writeOpHeader(conn, opRepDevlist, 0); err != nil {
		return err
	}
	if err := binary.Write(conn, binary.BigEndian, uint32(len(infos))); err != nil {
		return err
	}
	for _, info := range infos {
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

func handleImport(ctx context.Context, conn io.ReadWriter, p Provider) error {
	var busid [32]byte
	if _, err := io.ReadFull(conn, busid[:]); err != nil {
		return fmt.Errorf("reading import busid: %w", err)
	}
	requested := string(busid[:clen(busid[:])])
	dev, err := p.Open(requested)
	if err != nil {
		logrus.WithError(err).Warnf("usbip: import for busid %q rejected", requested)
		return writeOpHeader(conn, opRepImport, 1)
	}
	defer dev.Close()
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

// urbJob is a single CMD_SUBMIT routed to its endpoint's worker goroutine.
type urbJob struct {
	ctx     context.Context
	h       urbHeader
	outData []byte
	entry   *inflight
}

// epQueue is an unbounded FIFO of URB jobs for one endpoint. It is unbounded on
// purpose: the reader must never block enqueuing, because it has to stay free to
// read a CMD_UNLINK that cancels a worker currently blocked in an IN transfer. A
// bounded channel would deadlock if more URBs were queued than its capacity.
type epQueue struct {
	mu     sync.Mutex
	cond   *sync.Cond
	jobs   []*urbJob
	closed bool
}

func newEPQueue() *epQueue {
	q := &epQueue{}
	q.cond = sync.NewCond(&q.mu)
	return q
}

func (q *epQueue) push(j *urbJob) {
	q.mu.Lock()
	q.jobs = append(q.jobs, j)
	q.cond.Signal()
	q.mu.Unlock()
}

func (q *epQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.cond.Broadcast()
	q.mu.Unlock()
}

// pop returns the next job in FIFO order, blocking until one is available;
// ok is false once the queue is closed and fully drained.
func (q *epQueue) pop() (*urbJob, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.jobs) == 0 && !q.closed {
		q.cond.Wait()
	}
	if len(q.jobs) == 0 {
		return nil, false
	}
	j := q.jobs[0]
	q.jobs[0] = nil
	q.jobs = q.jobs[1:]
	return j, true
}

// urbLoop reads URB commands until the connection closes or ctx is cancelled.
// CMD_SUBMIT requests are routed to a per-endpoint worker so transfers on one
// endpoint complete in submission order — real hardware is FIFO per endpoint and
// the guest's mt76 RX ring asserts it (out-of-order completions corrupt the ring,
// stalling firmware load and hanging detach). Different endpoints run on separate
// workers so a blocking IN on one does not stall another. The inbound stream is
// read synchronously to stay frame-aligned and responsive to CMD_UNLINK.
func urbLoop(ctx context.Context, conn io.ReadWriter, dev Device) error {
	var writeMu sync.Mutex
	var stateMu sync.Mutex
	pending := map[uint32]*inflight{}
	workers := map[uint32]*epQueue{}
	var wg sync.WaitGroup

	// sessionCtx is cancelled either by the parent ctx or by a worker that finds
	// the host device unplugged. Cancelling it closes the connection, which
	// unblocks the read loop below so the whole session tears down; dropping the
	// vsock socket makes the guest's vhci-hcd release the imported port.
	sessionCtx, endSession := context.WithCancel(ctx)
	defer endSession()
	if c, ok := conn.(io.Closer); ok {
		go func() {
			<-sessionCtx.Done()
			_ = c.Close()
		}()
	}

	// A failing URB only reveals an unplug while the guest is actively driving the
	// device, and libusb's error code for a disconnect varies by platform. Poll
	// host presence directly so an unplug is caught within ~1s even for an idle
	// device, while an unresponsive-but-present device is left attached.
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-sessionCtx.Done():
				return
			case <-t.C:
				if dev.Gone() {
					logrus.Infof("usbip: host device %s unplugged; ending session", dev.Info().Busid)
					endSession()
					return
				}
			}
		}
	}()

	// Defers run LIFO: wg.Wait (registered first) runs last, after the cleanup
	// below cancels every in-flight transfer and closes every worker queue so the
	// workers can exit. Without this, a worker blocked in a gousb IN transfer when
	// the connection closes (e.g. on detach) would never return, wg.Wait would
	// hang, and the claimed host device would never be released.
	defer wg.Wait()
	defer func() {
		stateMu.Lock()
		for _, e := range pending {
			e.cancel()
		}
		for _, q := range workers {
			q.close()
		}
		stateMu.Unlock()
	}()

	worker := func(q *epQueue) {
		defer wg.Done()
		for {
			job, ok := q.pop()
			if !ok {
				return
			}
			status, actual, inData := doSubmit(job.ctx, dev, job.h, job.outData)

			stateMu.Lock()
			unlinked := job.entry.unlinked
			delete(pending, job.h.Seqnum)
			stateMu.Unlock()
			job.entry.cancel()

			// The guest already cancelled an unlinked URB via CMD_UNLINK and
			// received a RET_UNLINK; do not also send a RET_SUBMIT for it.
			if !unlinked {
				writeMu.Lock()
				if err := writeRetSubmit(conn, job.h, status, actual, inData); err != nil {
					logrus.WithError(err).Debug("usbip: writing ret_submit failed")
				}
				writeMu.Unlock()
			}

			if status == -errnoENODEV {
				// Host device physically unplugged; end the session so the guest
				// releases its vhci port rather than retrying a dead device.
				endSession()
			}
		}
	}

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

			urbCtx, cancel := context.WithCancel(sessionCtx)
			entry := &inflight{cancel: cancel}

			// Endpoint 0 is a single bidirectional control pipe (one worker);
			// every other endpoint number is a distinct IN/OUT pair, so the
			// direction bit selects a separate worker.
			key := h.Ep << 1
			if h.Ep != 0 {
				key |= h.Direction & 1
			}

			stateMu.Lock()
			pending[h.Seqnum] = entry
			q, ok := workers[key]
			if !ok {
				q = newEPQueue()
				workers[key] = q
				wg.Add(1)
				go worker(q)
			}
			stateMu.Unlock()

			q.push(&urbJob{ctx: urbCtx, h: h, outData: outData, entry: entry})

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
		if errors.Is(err, ErrDeviceGone) {
			logrus.WithError(err).Debugf("usbip: device gone on ep %d", h.Ep)
			return -errnoENODEV, 0, nil
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
