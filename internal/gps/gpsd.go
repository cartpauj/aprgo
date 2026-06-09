package gps

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"time"

	"aprgo/internal/state"
)

// gpsdReport is the union of the gpsd JSON objects we care about. gpsd emits
// newline-delimited JSON, one object per line, dispatched on "class". Position
// fields are *absent* (not zero) when there's no fix, so they're pointers.
type gpsdReport struct {
	Class string `json:"class"`

	// TPV
	Mode  int      `json:"mode"`
	Lat   *float64 `json:"lat"`
	Lon   *float64 `json:"lon"`
	Track *float64 `json:"track"`
	Speed *float64 `json:"speed"` // m/s

	// SKY
	USat *int     `json:"uSat"`
	HDOP *float64 `json:"hdop"`
	Sats []struct {
		Used bool `json:"used"`
	} `json:"satellites"`

	// ERROR
	Message string `json:"message"`
}

// sessionGPSD connects to gpsd, starts a JSON watch, and streams reports into
// the live fix. Returns on error / disconnect / cancel.
func (g *GPS) sessionGPSD(ctx context.Context, snap state.State) error {
	addr := snap.GPSDAddr
	if addr == "" {
		addr = state.DefaultGPSDAddr
	}
	d := net.Dialer{Timeout: 10 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("dial gpsd %s: %w", addr, err)
	}
	defer conn.Close()
	// Unblock the read on cancel; the done channel lets the watcher exit when
	// this session returns normally, so reconnects don't leak goroutines.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
		case <-done:
		}
		_ = conn.Close()
	}()

	// Start the JSON stream. gpsd accepts the command newline-terminated.
	if _, err := conn.Write([]byte("?WATCH={\"enable\":true,\"json\":true};\n")); err != nil {
		return fmt.Errorf("gpsd watch: %w", err)
	}
	g.setConnected(addr)
	defer g.setDisconnected()

	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 4096), 256*1024) // SKY lines can be long
	for sc.Scan() {
		if ctx.Err() != nil {
			return nil
		}
		var r gpsdReport
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			continue // ignore non-JSON / partial lines
		}
		g.applyGPSD(r)
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil {
		return err
	}
	return fmt.Errorf("gpsd stream ended")
}

func (g *GPS) applyGPSD(r gpsdReport) {
	g.mu.Lock()
	defer g.mu.Unlock()

	switch r.Class {
	case "SKY":
		if r.USat != nil {
			g.lastFix.Sats = *r.USat
		} else if len(r.Sats) > 0 {
			used := 0
			for _, s := range r.Sats {
				if s.Used {
					used++
				}
			}
			g.lastFix.Sats = used
		}
		if r.HDOP != nil {
			g.lastFix.HDOP = *r.HDOP
		}
	case "TPV":
		g.lastFix.Mode = r.Mode
		if r.Track != nil {
			g.lastFix.Track = *r.Track
		}
		if r.Speed != nil {
			g.lastFix.Speed = *r.Speed
		}
		// A usable fix requires mode >= 2 (2D/3D) and present coordinates.
		if r.Mode >= 2 && r.Lat != nil && r.Lon != nil {
			g.lastFix.Lat = *r.Lat
			g.lastFix.Lon = *r.Lon
			g.lastFix.At = time.Now()
			g.locked = true
			g.haveFix = true
		} else {
			g.locked = false
		}
	}
}
