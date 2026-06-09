package gps

import (
	"bufio"
	"context"
	"log"
	"net"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Device is a serial port that probing confirmed is emitting valid NMEA — a
// real GPS receiver, not arbitrary serial hardware. Only verified devices are
// offered in the settings picker.
type Device struct {
	Path   string // /dev/ttyACM0
	Label  string // friendly description for the dropdown
	Baud   int    // baud at which NMEA was confirmed (0 = USB CDC, rate irrelevant)
	Locked bool   // had a position fix during the probe
	Sats   int    // satellites seen during the probe
	Hint   string // by-id hint (e.g. "u-blox") when present
}

// usbGlobs are USB-attached serial GPS units — probed first because they're
// the common case and don't need a baud sweep (CDC-ACM ignores baud).
var usbGlobs = []string{
	"/dev/ttyACM*",
	"/dev/ttyUSB*",
}

// uartGlobs are GPIO/UART ports (Pi HATs). Probed only when no USB GPS turns
// up, because on a Pi /dev/serial0 + /dev/ttyAMA0 are frequently the console
// or Bluetooth UART — sweeping bauds on them is slow and pointless when a USB
// receiver is already present. Prefer the stable /dev/serial0 symlink.
var uartGlobs = []string{
	"/dev/serial0",
	"/dev/serial1",
	"/dev/ttyAMA*",
}

// nmeaBauds is the sweep order for real UARTs. 9600 is by far the most common
// default; 4800 is the legacy NMEA rate; the high rates are common u-blox
// configs. USB CDC-ACM devices ignore baud entirely, so we probe them once.
var nmeaBauds = []int{9600, 4800, 38400, 115200}

// minSentencesToVerify is how many checksum-valid, recognised NMEA sentences a
// device must emit during a probe to be accepted. A locked or acquiring
// receiver emits several per second, so this is reached within ~1 s; random
// serial noise essentially never passes even one checksum.
const minSentencesToVerify = 3

// ScanDevices enumerates candidate serial ports, probes each, and returns only
// those confirmed to be GPS receivers. Paths in exclude (e.g. the configured
// TNC device) are skipped so we never disturb the radio link. Probing is
// bounded by perDevice; the whole scan respects ctx.
func ScanDevices(ctx context.Context, exclude []string, perDevice time.Duration) []Device {
	if perDevice <= 0 {
		perDevice = 3 * time.Second
	}
	skip := map[string]bool{}
	for _, e := range exclude {
		if e != "" {
			skip[resolve(e)] = true
		}
	}
	hints := byIDHints()

	var out []Device
	seen := map[string]bool{}
	probeGlobs := func(globs []string) {
		for _, glob := range globs {
			matches, _ := filepath.Glob(glob)
			sort.Strings(matches)
			for _, path := range matches {
				real := resolve(path)
				if seen[real] || skip[real] || skip[path] {
					continue
				}
				seen[real] = true
				if ctx.Err() != nil {
					return
				}
				if dev, ok := probe(ctx, path, perDevice, hints[real]); ok {
					log.Printf("gps scan: %s verified (%s)", path, dev.Label)
					out = append(out, dev)
				} else {
					log.Printf("gps scan: %s not a GPS (no valid NMEA)", path)
				}
			}
		}
	}

	probeGlobs(usbGlobs)
	// Only probe the GPIO/UART ports if no USB GPS was found — avoids slow,
	// pointless baud sweeps on the Pi's console/Bluetooth UART.
	if len(out) == 0 {
		probeGlobs(uartGlobs)
	}
	log.Printf("gps scan: %d candidate(s) checked, %d GPS device(s) found (excluded: %v)", len(seen), len(out), exclude)
	return out
}

// probe opens a single device and reads briefly, deciding whether it's a GPS.
// For USB CDC-ACM (ttyACM*) baud is irrelevant so it probes once; for real
// UARTs it sweeps common bauds and stops at the first that yields valid NMEA.
func probe(ctx context.Context, path string, budget time.Duration, hint string) (Device, bool) {
	bauds := nmeaBauds
	if strings.Contains(path, "ttyACM") {
		bauds = []int{9600} // USB CDC: line rate doesn't matter
	}
	per := budget / time.Duration(len(bauds))
	if per < 800*time.Millisecond {
		per = 800 * time.Millisecond
	}
	for _, baud := range bauds {
		if ctx.Err() != nil {
			return Device{}, false
		}
		ok, locked, sats := readProbe(ctx, path, baud, per)
		if ok {
			d := Device{Path: path, Baud: baud, Locked: locked, Sats: sats, Hint: hint}
			if strings.Contains(path, "ttyACM") {
				d.Baud = 0 // signal "rate irrelevant"
			}
			d.Label = label(path, hint, locked, sats, d.Baud)
			return d, true
		}
	}
	return Device{}, false
}

// readProbe opens path at baud and reads for dur, counting valid NMEA. Returns
// whether the threshold was met, plus whether a fix was seen and the sat count.
func readProbe(ctx context.Context, path string, baud int, dur time.Duration) (ok, locked bool, sats int) {
	f, err := openGPSSerial(path, baud)
	if err != nil {
		log.Printf("gps scan: open %s @ %d failed: %v", path, baud, err)
		return false, false, 0
	}
	defer f.Close()
	// Bound the read: close the fd after dur (or on ctx cancel) to unblock the
	// scanner, since tty reads don't honor deadlines reliably.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-time.After(dur):
		case <-ctx.Done():
		case <-done:
		}
		_ = f.Close()
	}()

	count := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 4096), 64*1024)
	for sc.Scan() {
		d, valid := parseSentence(sc.Text())
		if !valid {
			continue
		}
		count++
		if d.haveSats && d.sats > sats {
			sats = d.sats
		}
		if (d.haveQuality && d.quality >= 1) || (d.haveStatus && d.valid) {
			locked = true
		}
		// A few checksum-valid, recognised NMEA sentences are conclusive
		// evidence of a GPS — return immediately so the scan stays snappy
		// (a USB receiver hits this in well under a second).
		if count >= minSentencesToVerify {
			return true, locked, sats
		}
	}
	return count >= minSentencesToVerify, locked, sats
}

// GpsdAvailable reports whether a gpsd daemon is reachable at addr. It dials,
// reads one line, and checks for the VERSION banner gpsd pushes on connect.
// The dial itself triggers gpsd's systemd socket activation on Debian/Pi.
func GpsdAvailable(ctx context.Context, addr string) bool {
	if addr == "" {
		addr = "127.0.0.1:2947"
	}
	d := net.Dialer{Timeout: 2 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return false
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	sc := bufio.NewScanner(conn)
	if sc.Scan() {
		return strings.Contains(sc.Text(), "\"class\":\"VERSION\"")
	}
	return false
}

// ---- helpers ----

func resolve(path string) string {
	if r, err := filepath.EvalSymlinks(path); err == nil {
		return r
	}
	return path
}

// byIDHints maps resolved device paths to a friendly hint string when the
// /dev/serial/by-id symlink name suggests a GPS (u-blox / GPS / GNSS).
func byIDHints() map[string]string {
	hints := map[string]string{}
	entries, _ := filepath.Glob("/dev/serial/by-id/*")
	for _, e := range entries {
		real, err := filepath.EvalSymlinks(e)
		if err != nil {
			continue
		}
		base := filepath.Base(e)
		low := strings.ToLower(base)
		switch {
		case strings.Contains(low, "u-blox") || strings.Contains(low, "ublox"):
			hints[real] = "u-blox"
		case strings.Contains(low, "gnss") || strings.Contains(low, "gps"):
			hints[real] = "GPS/GNSS"
		}
	}
	return hints
}

func label(path, hint string, locked bool, sats, baud int) string {
	var b strings.Builder
	b.WriteString(filepath.Base(path))
	if hint != "" {
		b.WriteString(" · " + hint)
	}
	if baud != 0 {
		b.WriteString(" · " + strconv.Itoa(baud) + " baud")
	}
	if locked {
		b.WriteString(" · fix")
	} else {
		b.WriteString(" · acquiring")
	}
	if sats > 0 {
		b.WriteString(" (" + strconv.Itoa(sats) + " sats)")
	}
	return b.String()
}
