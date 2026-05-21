//go:build linux

package rf

// serialConn wraps an os.File holding a tty-class device (`/dev/ttyUSB*`,
// `/dev/ttyACM*`, `/dev/rfcomm*`) and makes Read interruptible by context
// cancellation. Without this, a goroutine blocked in `os.File.Read` on a
// rfcomm device cannot be unblocked by another goroutine calling Close —
// Linux `close(2)` doesn't interrupt a pending `read(2)` on a tty, and
// the rfcomm driver only delivers HUP after bluez tears down the
// underlying L2CAP channel, which costs ~10s. By polling with a short
// timeout we cap the cancellation latency to ~200ms.
//
// Implementation: the fd is set non-blocking so reads return EAGAIN
// immediately when there's no data. We then `poll(2)` with a 200ms
// timeout, check the context between polls, and read when POLLIN fires.

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

const serialPollInterval = 200 // ms

type serialConn struct {
	ctx    context.Context
	f      *os.File
	closed chan struct{}
	once   sync.Once
}

// newSerialConn wraps f for context-aware I/O. f is set non-blocking; the
// caller still owns f's lifecycle (close it when done).
//
// Shutdown latency: the Read loop polls with a 200ms timeout and re-checks
// ctx between calls, so the upper bound for waking from a blocked read on
// ctx cancel is ~200ms. We previously launched a watcher goroutine intended
// to "wake the poll loop the instant ctx is cancelled" but it had no body —
// it received from `ctx.Done()` or `c.closed` and did nothing useful (the
// comment explicitly admitted closing the file would race the syscall).
// Deleted. The 200ms tick is the actual cancellation bound.
func newSerialConn(ctx context.Context, f *os.File) (*serialConn, error) {
	if err := unix.SetNonblock(int(f.Fd()), true); err != nil {
		return nil, err
	}
	return &serialConn{ctx: ctx, f: f, closed: make(chan struct{})}, nil
}

// Read blocks until data is available or ctx is cancelled. Loops on poll
// with a short timeout, checking ctx between calls.
func (c *serialConn) Read(p []byte) (int, error) {
	fd := int32(c.f.Fd())
	for {
		if c.ctx.Err() != nil {
			return 0, c.ctx.Err()
		}
		fds := []unix.PollFd{{Fd: fd, Events: unix.POLLIN | unix.POLLHUP | unix.POLLERR}}
		_, err := unix.Poll(fds, serialPollInterval)
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return 0, err
		}
		if fds[0].Revents&(unix.POLLHUP|unix.POLLERR) != 0 {
			return 0, io.EOF
		}
		if fds[0].Revents&unix.POLLIN == 0 {
			// Timeout. Loop to recheck ctx.
			continue
		}
		n, err := c.f.Read(p)
		if err != nil {
			if errors.Is(err, unix.EAGAIN) {
				continue // spurious POLLIN — try again
			}
			return n, err
		}
		return n, nil
	}
}

// Write is a passthrough — writes to a tty rarely block long enough to
// matter and converting them to non-blocking would complicate the KISS
// frame writer significantly. The TX inter-frame spacing already bounds
// write cadence to ~1/sec.
func (c *serialConn) Write(p []byte) (int, error) {
	return c.f.Write(p)
}

// Close releases the context-watcher goroutine and closes the underlying
// file descriptor. Idempotent — safe to call multiple times. Callers should
// NOT separately close the *os.File they handed to newSerialConn; doing so
// would double-close the fd. The only exception is when newSerialConn
// itself returned an error (rare — see the error branch in rf.go), in
// which case the caller still owns the unwrapped file.
func (c *serialConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.f.Close()
}
