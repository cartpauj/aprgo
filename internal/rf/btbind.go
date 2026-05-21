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
	"aprgo/internal/tnc"
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

	// discoveryRan ensures the "adopt the kernel's current rfcomm MAC"
	// fallback only fires once, at startup, before we begin spawning
	// `rfcomm connect` subprocesses. After that, running `rfcomm` while
	// our own bind is mid-handshake can transiently report the local
	// adapter MAC instead of the remote — we'd then update state with
	// nonsense and trigger a feedback loop.
	discoveryRan bool
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

	// One-shot startup self-heal: if state has us pointed at an rfcomm
	// device but no MAC, ask the kernel what's already bound there. The
	// kernel state is stable here because we haven't spawned any
	// `rfcomm connect` subprocesses yet (discoveryRan gates this to the
	// first reconcile call only). After that, running `rfcomm` while our
	// own bind is mid-handshake can transiently report the local adapter
	// MAC instead of the remote — adopting that would brick things.
	b.mu.Lock()
	first := !b.discoveryRan
	b.discoveryRan = true
	b.mu.Unlock()
	if first && snap.TNCKind == state.TNCSerial && rfcommDevRE.MatchString(dev) && !btAddrRE.MatchString(addr) {
		probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		discovered, _ := tnc.CurrentRfcommMAC(probeCtx, dev)
		cancel()
		if discovered != "" {
			log.Printf("rf: btSupervisor: discovered MAC %s on %s — adopting into state", discovered, dev)
			_ = b.st.Update(func(s *state.State) error {
				s.TNCAddr = discovered
				return nil
			})
			// state.Update fires a notify which schedules another
			// reconcile — let that one re-evaluate with the fresh MAC.
			return
		}
		log.Printf("rf: btSupervisor: inactive (TNCSerial=%s but no TNCAddr — assuming external rfcomm management, or operator needs to configure pairing)", dev)
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

// disableBTSniff clears SNIFF from hci0's default link policy. SNIFF causes
// the ACL to enter a low-power polled mode during idle; on flaky controllers
// (notably Marvell SD8887 combo cards) the transitions in/out of sniff can
// drop the link, forcing a full page+L2CAP+RFCOMM reconnect every few
// minutes. The reconnect storm then poisons WiFi/BT coex on shared-SDIO
// adapters. Disabling sniff costs ~microamps of idle power — negligible for
// an iGate — and is safe on every controller. Failure is logged but ignored;
// it's best-effort.
func disableBTSniff(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, "hciconfig", "hci0", "lp", "rswitch,hold").CombinedOutput()
	if err != nil {
		log.Printf("rf: hciconfig hci0 lp rswitch,hold failed: %v (output: %s) — leaving adapter default policy unchanged", err, strings.TrimSpace(string(out)))
		return
	}
	log.Printf("rf: disabled SNIFF on hci0 default link policy (Marvell/coex workaround)")
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
	disableBTSniff(ctx)
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
