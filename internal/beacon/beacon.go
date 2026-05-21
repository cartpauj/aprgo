// Package beacon emits position/status/etc beacons on independent schedules.
//
// Each entry in state.Beacons is fired separately when (a) Enabled=true,
// (b) station TXEnable=true, (c) interval >= minInterval, and (d) the
// wall-clock interval has elapsed since this specific beacon's last firing.
// A small ±10% jitter is applied per beacon to prevent synchronized
// transmissions across multiple aprgo instances or multiple beacons.
package beacon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"strings"
	"sync"
	"time"

	"aprgo/internal/ax25"
	"aprgo/internal/bus"
	"aprgo/internal/rf"
	"aprgo/internal/state"
)

// minInterval is the floor for any beacon (10 min — APRS community norm for
// fixed stations). Values below this are silently bumped up.
const minInterval = 10 * time.Minute

// ISSender is the subset of the APRS-IS client we use to emit beacons in
// IS-only mode. Defined as an interface to keep the beacon package free
// of an igate import (avoids cyclic-dep risk if igate ever grows callbacks
// back into beacon territory).
type ISSender interface {
	Send(packet string) error
}

// Beacon runs the periodic beacon scheduler.
type Beacon struct {
	st  *state.Store
	rf  *rf.RF
	is  ISSender
	bus *bus.Bus

	// Per-beacon firing state, keyed by beacon name. Recreated when the list
	// changes (a beacon removed from state no longer has an entry).
	fires map[string]*fireState

	// lastFired tracks the most recent successful TX time per beacon name,
	// for the Settings page "Last fired: X ago" indicator. In-memory only;
	// lost on restart (correct — the only meaningful answer is "since
	// process boot"). Guarded by lastMu because the supervisor's transmit
	// goroutine writes while the HTTP handler reads.
	lastMu    sync.Mutex
	lastFired map[string]time.Time
}

type fireState struct {
	last   time.Time
	jitter time.Duration
}

func New(st *state.Store, r *rf.RF, b *bus.Bus) *Beacon {
	return &Beacon{st: st, rf: r, bus: b,
		fires:     make(map[string]*fireState),
		lastFired: make(map[string]time.Time),
	}
}

// recordFired stamps `name` with the current time after a successful TX.
func (b *Beacon) recordFired(name string) {
	b.lastMu.Lock()
	b.lastFired[name] = time.Now()
	b.lastMu.Unlock()
}

// LastFired returns a copy of the per-beacon last-TX times. Empty entries
// (beacons that haven't fired yet this run) are simply absent.
func (b *Beacon) LastFired() map[string]time.Time {
	b.lastMu.Lock()
	defer b.lastMu.Unlock()
	out := make(map[string]time.Time, len(b.lastFired))
	for k, v := range b.lastFired {
		out[k] = v
	}
	return out
}

// Run blocks until ctx is cancelled.
func (b *Beacon) Run(ctx context.Context) {
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			snap := b.st.Snapshot()
			if !snap.TXEnable || snap.Callsign == "" {
				continue
			}
			for _, cfg := range snap.Beacons {
				if !cfg.Enabled {
					continue
				}
				interval := cfg.Interval()
				if interval < minInterval {
					continue
				}
				fs, ok := b.fires[cfg.Name]
				if !ok {
					fs = &fireState{last: now} // first firing happens one full interval after registration
					b.fires[cfg.Name] = fs
					continue
				}
				if now.Sub(fs.last) < interval+fs.jitter {
					continue
				}
				if err := b.transmit(snap, cfg); err != nil {
					if !errors.Is(err, rf.ErrTXDisabled) {
						log.Printf("beacon[%s]: %v", cfg.Name, err)
					}
					continue
				}
				fs.last = now
				j := int64(interval) / 10
				if j > 0 {
					fs.jitter = time.Duration(rand.Int63n(2*j+1) - j)
				}
			}
			// Prune fireState entries whose beacon no longer exists.
			alive := map[string]bool{}
			for _, c := range snap.Beacons {
				alive[c.Name] = true
			}
			for name := range b.fires {
				if !alive[name] {
					delete(b.fires, name)
				}
			}
		}
	}
}

// TransmitNow finds an enabled beacon by name and fires it immediately,
// bypassing the schedule. Used to satisfy `?APRS?` general queries (which
// request a position + status from the queried station). Returns an error
// if no enabled beacon with that name exists.
func (b *Beacon) TransmitNow(name string) error {
	snap := b.st.Snapshot()
	for _, cfg := range snap.Beacons {
		if cfg.Name == name && cfg.Enabled {
			return b.transmit(snap, cfg)
		}
	}
	return fmt.Errorf("no enabled beacon named %q", name)
}

func (b *Beacon) transmit(snap state.State, cfg state.Beacon) error {
	dest := cfg.Dest
	if dest == "" {
		dest = state.DefaultBeaconDest
	}
	// Per-beacon callsign override falls back to the station's primary.
	src := cfg.Callsign
	if src == "" {
		src = snap.Callsign
	}
	if src == "" {
		return fmt.Errorf("beacon %s has no callsign", cfg.Name)
	}
	infoStr := cfg.ComposeInfo(snap.Lat, snap.Lon)
	if infoStr == "" {
		return fmt.Errorf("beacon %s has no position (set station lat/lon first)", cfg.Name)
	}
	info := []byte(strings.NewReplacer("\r", "", "\n", "").Replace(infoStr))
	raw, err := ax25.EncodeUIFrame(src, dest, cfg.Path, info)
	if err != nil {
		return err
	}
	if err := b.rf.TX(raw); err != nil {
		return err
	}
	if b.bus != nil {
		if frame, ferr := ax25.FromAX25(raw, ax25.SrcTX, "beacon:"+cfg.Name); ferr == nil {
			b.bus.Frames.Publish(frame)
		}
	}
	b.recordFired(cfg.Name)
	return nil
}
