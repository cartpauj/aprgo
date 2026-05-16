package rf

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"aprgo/internal/state"
)

// btSupervisor keeps `rfcomm connect` alive whenever the configured TNC is a
// Bluetooth serial device (state.TNCKind=serial, state.TNCSerial=/dev/rfcommN,
// state.TNCAddr=AA:BB:...). Re-runs the binding on state changes.
type btSupervisor struct {
	st *state.Store

	mu       sync.Mutex
	cancel   context.CancelFunc
	wantAddr string
	wantDev  string
	wantChan int
}

func newBTSupervisor(st *state.Store) *btSupervisor {
	return &btSupervisor{st: st}
}

var rfcommDevRE = regexp.MustCompile(`^/dev/rfcomm(\d+)$`)
var btAddrRE = regexp.MustCompile(`^[0-9A-Fa-f]{2}(:[0-9A-Fa-f]{2}){5}$`)

// Run watches state and starts/stops the rfcomm child process to match.
// Blocks until ctx is cancelled.
func (b *btSupervisor) Run(ctx context.Context) {
	stateCh, cancelSub := b.st.Subscribe()
	defer cancelSub()
	b.reconcile(ctx)
	for {
		select {
		case <-ctx.Done():
			b.stop()
			return
		case <-stateCh:
			b.reconcile(ctx)
		}
	}
}

// reconcile compares state to what the supervisor is currently running and
// restarts the child if anything relevant changed.
func (b *btSupervisor) reconcile(ctx context.Context) {
	snap := b.st.Snapshot()
	addr := strings.ToUpper(snap.TNCAddr)
	dev := snap.TNCSerial
	ch := snap.TNCChannel
	if ch <= 0 {
		ch = 1
	}
	want := snap.TNCKind == state.TNCSerial && rfcommDevRE.MatchString(dev) && btAddrRE.MatchString(addr)

	b.mu.Lock()
	defer b.mu.Unlock()
	if !want {
		if b.cancel != nil {
			b.cancel()
			b.cancel = nil
		}
		return
	}
	// Already running with the same params?
	if b.cancel != nil && b.wantAddr == addr && b.wantDev == dev && b.wantChan == ch {
		return
	}
	if b.cancel != nil {
		b.cancel()
		b.cancel = nil
	}
	b.wantAddr = addr
	b.wantDev = dev
	b.wantChan = ch

	cctx, cancel := context.WithCancel(ctx)
	b.cancel = cancel
	go b.supervise(cctx, addr, dev, ch)
}

func (b *btSupervisor) stop() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cancel != nil {
		b.cancel()
		b.cancel = nil
	}
}

// supervise runs `rfcomm connect <idx> <addr> <channel>` and restarts it
// whenever it exits, until ctx is cancelled.
func (b *btSupervisor) supervise(ctx context.Context, addr, dev string, channel int) {
	m := rfcommDevRE.FindStringSubmatch(dev)
	if m == nil {
		return
	}
	idx := m[1]
	backoff := time.Second
	for ctx.Err() == nil {
		// Release any stale binding first (idempotent — failure is fine).
		releaseCtx, releaseCancel := context.WithTimeout(ctx, 3*time.Second)
		_ = exec.CommandContext(releaseCtx, "rfcomm", "release", idx).Run()
		releaseCancel()

		log.Printf("rf: rfcomm connect %s %s %d", idx, addr, channel)
		cmd := exec.CommandContext(ctx, "rfcomm", "connect", idx, addr, strconv.Itoa(channel))
		cmd.Stdout = nil
		cmd.Stderr = nil
		err := cmd.Run()
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.Printf("rf: rfcomm connect exited: %v (retry in %s)", err, backoff)
		} else {
			log.Printf("rf: rfcomm connect exited cleanly (retry in %s)", backoff)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 15*time.Second {
			backoff *= 2
		}
	}
}

// waitForDevice polls until `path` exists or ctx expires.
func waitForDevice(ctx context.Context, path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return fmt.Errorf("waiting for %s: timeout", path)
}
