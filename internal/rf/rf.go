// Package rf owns the radio-side I/O: serial KISS, Bluetooth RFCOMM (which is
// a serial TTY), and TCP KISS endpoints. It reads inbound bytes, splits KISS
// frames, decodes AX.25, and publishes ax25.Frame{Origin: SrcRF} on the bus.
// On the TX side it accepts AX.25 bytes via TX(), encodes KISS, writes to the
// device, and re-opens on error.
package rf

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"aprgo/internal/ax25"
	"aprgo/internal/bus"
	"aprgo/internal/state"
)

// ErrTXDisabled is returned by TX() when TXEnable is false in state.
var ErrTXDisabled = errors.New("rf: TX disabled in settings")

// ErrTXQueueFull is returned by TX() when the queue is saturated (64 deep).
var ErrTXQueueFull = errors.New("rf: TX queue full")

// ErrNoTNC is returned when no TNC is configured (kind==None).
var ErrNoTNC = errors.New("rf: no TNC configured")

const txQueueDepth = 64

// RF runs the loop: open device, read frames, publish to bus.Frames.
type RF struct {
	st  *state.Store
	bus *bus.Bus
	bt  *btSupervisor

	mu            sync.RWMutex
	connected     bool
	iface         string // device path or host:port for diagnostics
	lastError     string
	lastErrorAt   time.Time
	txCh          chan []byte
	sessionCancel context.CancelFunc
}

// LastError returns the most recent rf session error and its timestamp.
func (r *RF) LastError() (string, time.Time) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.lastError, r.lastErrorAt
}

func New(st *state.Store, b *bus.Bus) *RF {
	return &RF{
		st:   st,
		bus:  b,
		bt:   newBTSupervisor(st),
		txCh: make(chan []byte, txQueueDepth),
	}
}

// Connected reports whether we currently have a live RF session.
func (r *RF) Connected() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.connected
}

// IFace returns the human description of the current source (device or host).
func (r *RF) IFace() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.iface
}

// TX queues raw AX.25 bytes for transmission. KISS framing is added inside.
// Returns ErrTXDisabled / ErrTXQueueFull / ErrNoTNC as appropriate.
func (r *RF) TX(ax25Bytes []byte) error {
	snap := r.st.Snapshot()
	if snap.TNCKind == state.TNCNone {
		return ErrNoTNC
	}
	if !snap.TXEnable {
		return ErrTXDisabled
	}
	frame := ax25.EncodeKISS(ax25Bytes)
	select {
	case r.txCh <- frame:
		return nil
	default:
		return ErrTXQueueFull
	}
}

// Run is the main loop. Watches state for TNC config changes; when the kind /
// device / addr changes it drops the current connection and re-dials.
func (r *RF) Run(ctx context.Context) {
	// Bluetooth supervisor runs independently so the BT link stays up even when
	// the read/write session is bouncing.
	go r.bt.Run(ctx)

	stateCh, cancelSub := r.st.Subscribe()
	defer cancelSub()

	go func() {
		for snap := range stateCh {
			_ = snap
			r.dropSession()
		}
	}()

	backoff := time.Second
	for ctx.Err() == nil {
		snap := r.st.Snapshot()
		if snap.TNCKind == state.TNCNone {
			select {
			case <-time.After(2 * time.Second):
			case <-ctx.Done():
				return
			}
			continue
		}
		if err := r.session(ctx, snap); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("rf: session: %v (retry in %s)", err, backoff)
			r.mu.Lock()
			r.lastError = err.Error()
			r.lastErrorAt = time.Now()
			r.mu.Unlock()
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
	}
}

func (r *RF) dropSession() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sessionCancel != nil {
		r.sessionCancel()
		r.sessionCancel = nil
	}
}

// session opens the configured source and runs the read/write loop until error.
func (r *RF) session(parent context.Context, snap state.State) error {
	ctx, cancel := context.WithCancel(parent)
	r.mu.Lock()
	r.sessionCancel = cancel
	r.mu.Unlock()
	defer cancel()

	var rw io.ReadWriteCloser
	var iface string
	var err error
	switch snap.TNCKind {
	case state.TNCSerial:
		if snap.TNCSerial == "" {
			return errors.New("serial TNC selected but no device path set")
		}
		// If this is an RFCOMM device, the BT supervisor may still be bringing
		// it up. Give it up to 10 seconds to appear.
		if rfcommDevRE.MatchString(snap.TNCSerial) {
			if err := waitForDevice(ctx, snap.TNCSerial, 10*time.Second); err != nil {
				return err
			}
		}
		baud := snap.TNCBaud
		if baud == 0 && !rfcommDevRE.MatchString(snap.TNCSerial) {
			baud = 9600 // sensible default for real serial ports
		}
		rw, err = openSerial(snap.TNCSerial, baud)
		iface = snap.TNCSerial
	case state.TNCTCP:
		if snap.TNCAddr == "" {
			return errors.New("TCP TNC selected but no host:port set")
		}
		rw, err = openTCP(ctx, snap.TNCAddr)
		iface = snap.TNCAddr
	default:
		return ErrNoTNC
	}
	if err != nil {
		return err
	}
	defer rw.Close()

	r.mu.Lock()
	r.connected = true
	r.iface = iface
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.connected = false
		r.mu.Unlock()
	}()

	log.Printf("rf: connected to %s (%s)", iface, snap.TNCKind)

	// Drain TX queue in a goroutine while we read.
	writeDone := make(chan error, 1)
	go func() {
		writeDone <- r.writeLoop(ctx, rw)
	}()

	readErr := r.readLoop(ctx, rw, iface)
	cancel()
	// Close the underlying ReadWriteCloser so a wedged Write inside writeLoop
	// unblocks. exec.CommandContext-style ctx-cancel won't help with raw FD
	// blocking writes; closing the fd is the only reliable interrupt.
	_ = rw.Close()
	<-writeDone
	return readErr
}

func (r *RF) readLoop(ctx context.Context, rd io.Reader, iface string) error {
	split := ax25.KISSFrameSplitter{}
	defer split.Reset()
	buf := make([]byte, 4096)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		n, err := rd.Read(buf)
		if err != nil {
			return err
		}
		segs := split.Push(buf[:n])
		for _, seg := range segs {
			payload := ax25.DecodeKISS(seg)
			if payload == nil {
				continue
			}
			frame, ferr := ax25.FromAX25(payload, ax25.SrcRF, iface)
			if ferr != nil {
				continue
			}
			r.bus.Frames.Publish(frame)
		}
	}
}

// txMinSpacing is the minimum gap between successive RF writes. APRS courtesy
// is roughly one transmission per second on a shared channel; back-to-back
// writes step on other stations and mask collisions when our own bursts line up.
const txMinSpacing = 1 * time.Second

func (r *RF) writeLoop(ctx context.Context, wr io.Writer) error {
	var lastTX time.Time
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case b := <-r.txCh:
			if !lastTX.IsZero() {
				if wait := txMinSpacing - time.Since(lastTX); wait > 0 {
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-time.After(wait):
					}
				}
			}
			if _, err := wr.Write(b); err != nil {
				return err
			}
			lastTX = time.Now()
		}
	}
}

// openSerial opens a TTY device and puts it in raw mode at the given baud.
// baud=0 leaves the existing speed untouched (correct for RFCOMM).
func openSerial(path string, baud int) (io.ReadWriteCloser, error) {
	f, err := os.OpenFile(path, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if err := SetRaw(f, baud); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("set raw %s: %w", path, err)
	}
	return f, nil
}

// openTCP dials a host:port and returns the connection.
func openTCP(ctx context.Context, addr string) (io.ReadWriteCloser, error) {
	// Allow "host:port" or just "host" (default port 8001 = Direwolf default)
	if !strings.Contains(addr, ":") {
		addr += ":8001"
	}
	d := net.Dialer{Timeout: 10 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	return conn, nil
}
