package gps

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"aprgo/internal/state"
)

// liveWindow bounds how recently the receiver must have reported a fix for us
// to call it "locked right now". NMEA receivers emit RMC/GGA ~1 Hz and gpsd
// streams at the device cadence, so anything older than this means the feed
// has stalled or lost lock.
const liveWindow = 10 * time.Second

// Fix is a single position observation.
type Fix struct {
	Lat, Lon float64
	Mode     int       // 0/1 = no fix, 2 = 2D, 3 = 3D
	Sats     int       // satellites used in the solution
	HDOP     float64   // horizontal dilution of precision (lower = better)
	Speed    float64   // ground speed, m/s
	Track    float64   // course over ground, degrees true
	At       time.Time // when aprgo observed this fix
}

// Status is the operator-facing snapshot surfaced in the UI alongside the
// TNC and APRS-IS indicators.
type Status struct {
	Enabled     bool   // GPS is the configured position source
	Source      string // "nmea-serial" | "gpsd"
	Iface       string // device path or gpsd host:port
	Connected   bool   // transport is up (port open / gpsd connected)
	Locked      bool   // currently has a usable position fix
	HaveFix     bool   // we have ever seen a valid fix (last-known available)
	Fix         Fix    // most recent valid fix
	LastError   string // most recent session error
	LastErrorAt time.Time
}

// GPS supervises the configured position source and maintains the freshest
// fix. It mirrors rf.RF's connect/reconnect lifecycle: a single supervisor
// watches state, tears down and restarts the session when the GPS config
// changes, and reconnects with backoff on error.
type GPS struct {
	st *state.Store

	mu          sync.RWMutex
	enabled     bool
	source      string
	iface       string
	connected   bool
	locked      bool
	lastFix     Fix
	haveFix     bool
	lastError   string
	lastErrorAt time.Time
}

func New(st *state.Store) *GPS { return &GPS{st: st} }

// Status returns a copy of the current GPS status for the UI.
func (g *GPS) Status() Status {
	g.mu.RLock()
	defer g.mu.RUnlock()
	locked := g.locked && !g.lastFix.At.IsZero() && time.Since(g.lastFix.At) < liveWindow
	return Status{
		Enabled:     g.enabled,
		Source:      g.source,
		Iface:       g.iface,
		Connected:   g.connected,
		Locked:      locked,
		HaveFix:     g.haveFix,
		Fix:         g.lastFix,
		LastError:   g.lastError,
		LastErrorAt: g.lastErrorAt,
	}
}

// Position implements the beacon.PositionProvider interface. It returns the
// best available fix: lat/lon from the most recent valid fix, its age, whether
// the receiver is locked *right now*, and whether we've ever had a fix at all.
// The beacon layer decides how to apply fallback policy.
func (g *GPS) Position() (lat, lon float64, age time.Duration, locked, ok bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if !g.haveFix {
		return 0, 0, 0, false, false
	}
	age = time.Since(g.lastFix.At)
	locked = g.locked && age < liveWindow
	return g.lastFix.Lat, g.lastFix.Lon, age, locked, true
}

// Run is the supervisor loop. Blocks until ctx is cancelled.
func (g *GPS) Run(ctx context.Context) {
	stateCh, cancelSub := g.st.Subscribe()
	defer cancelSub()

	var (
		sessCancel context.CancelFunc
		sessWG     sync.WaitGroup
		curKey     string
	)
	stop := func() {
		if sessCancel != nil {
			sessCancel()
			sessWG.Wait()
			sessCancel = nil
		}
		g.setIdle()
	}
	apply := func(snap state.State) {
		key := configKey(snap)
		if key == curKey {
			return
		}
		stop()
		curKey = key
		if snap.PositionSource != state.PosGPS {
			return
		}
		sctx, cancel := context.WithCancel(ctx)
		sessCancel = cancel
		src := string(snap.GPSKind)
		if src == "" {
			src = string(state.GPSSerial)
		}
		g.setEnabled(src)
		sessWG.Add(1)
		go func(snap state.State) {
			defer sessWG.Done()
			g.supervise(sctx, snap)
		}(snap)
	}

	apply(g.st.Snapshot())
	for {
		select {
		case <-ctx.Done():
			stop()
			return
		case snap, ok := <-stateCh:
			if !ok {
				stop()
				<-ctx.Done()
				return
			}
			apply(snap)
		}
	}
}

// configKey collapses the GPS-relevant settings into a comparison key so the
// supervisor only restarts the session when something it cares about changes.
func configKey(s state.State) string {
	if s.PositionSource != state.PosGPS {
		return "off"
	}
	return fmt.Sprintf("%s|%s|%d|%s", s.GPSKind, s.GPSDevice, s.GPSBaud, s.GPSDAddr)
}

// supervise runs one source's session, reconnecting with capped backoff until
// its context is cancelled (config change or shutdown).
func (g *GPS) supervise(ctx context.Context, snap state.State) {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		err := g.session(ctx, snap)
		if ctx.Err() != nil {
			return // cancelled: not a real error
		}
		if err != nil {
			g.setError(err)
			log.Printf("gps: %v (retry in %s)", err, backoff.Round(time.Second))
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (g *GPS) session(ctx context.Context, snap state.State) error {
	switch snap.GPSKind {
	case state.GPSD:
		return g.sessionGPSD(ctx, snap)
	default:
		return g.sessionSerial(ctx, snap)
	}
}

// sessionSerial opens the local NMEA tty and reads until error / cancel.
func (g *GPS) sessionSerial(ctx context.Context, snap state.State) error {
	dev := snap.GPSDevice
	if dev == "" {
		return errors.New("no GPS device configured")
	}
	baud := snap.GPSBaud
	if baud == 0 {
		baud = state.DefaultGPSBaud
	}
	f, err := openGPSSerial(dev, baud)
	if err != nil {
		return err
	}
	// Close the fd to unblock the blocking read on cancel *or* when this
	// session returns (read error). The done channel makes the watcher exit
	// either way, so a flapping device can't leak a goroutine per reconnect.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
		case <-done:
		}
		_ = f.Close()
	}()
	g.setConnected(dev)
	defer g.setDisconnected()
	return g.readNMEA(ctx, bufio.NewScanner(f))
}

// readNMEA consumes NMEA lines, merging each into the live fix. Returns when
// the scanner ends (device closed / unplugged) or ctx is cancelled.
func (g *GPS) readNMEA(ctx context.Context, sc *bufio.Scanner) error {
	sc.Buffer(make([]byte, 0, 4096), 64*1024)
	for sc.Scan() {
		if ctx.Err() != nil {
			return nil
		}
		d, ok := parseSentence(sc.Text())
		if !ok {
			continue
		}
		g.applySentence(d)
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil {
		return err
	}
	return errors.New("GPS stream ended (device disconnected?)")
}

// applySentence folds a single decoded sentence into the live fix state.
func (g *GPS) applySentence(d sentenceData) {
	g.mu.Lock()
	defer g.mu.Unlock()

	// Carry forward sat count / HDOP / mode from GSA & GGA as they arrive.
	if d.haveSats {
		g.lastFix.Sats = d.sats
	}
	if d.haveHDOP {
		g.lastFix.HDOP = d.hdop
	}
	if d.haveMode {
		g.lastFix.Mode = d.mode
	}
	if d.haveSpeed {
		g.lastFix.Speed = d.speedKnots * knotsToMS
	}
	if d.haveTrack {
		g.lastFix.Track = d.track
	}

	// Lock determination: a GGA with quality>=1 or an RMC/GLL with status A,
	// carrying a position, is a usable fix.
	locked := (d.haveQuality && d.quality >= 1) || (d.haveStatus && d.valid)
	if locked && d.havePos {
		g.lastFix.Lat = d.lat
		g.lastFix.Lon = d.lon
		g.lastFix.At = time.Now()
		if g.lastFix.Mode < 2 {
			g.lastFix.Mode = 2 // GGA-only receivers never emit GSA; treat lock as >=2D
		}
		g.locked = true
		g.haveFix = true
	} else if d.haveStatus || d.haveQuality {
		// An explicit no-fix report (status V / quality 0) means we've lost lock.
		g.locked = false
	}
}

// ---- status setters ----

func (g *GPS) setEnabled(source string) {
	g.mu.Lock()
	g.enabled = true
	g.source = source
	g.mu.Unlock()
}

func (g *GPS) setConnected(iface string) {
	g.mu.Lock()
	g.connected = true
	g.iface = iface
	g.lastError = ""
	g.mu.Unlock()
}

func (g *GPS) setDisconnected() {
	g.mu.Lock()
	g.connected = false
	g.locked = false
	g.mu.Unlock()
}

func (g *GPS) setError(err error) {
	g.mu.Lock()
	g.connected = false
	g.locked = false
	g.lastError = err.Error()
	g.lastErrorAt = time.Now()
	g.mu.Unlock()
}

// setIdle clears everything when GPS is disabled or the session stops.
func (g *GPS) setIdle() {
	g.mu.Lock()
	g.enabled = false
	g.connected = false
	g.locked = false
	g.source = ""
	g.iface = ""
	g.mu.Unlock()
}
