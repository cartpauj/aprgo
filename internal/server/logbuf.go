package server

import (
	"io"
	"strings"
	"sync"
	"time"
)

// LogBuffer is an io.Writer that retains the most recent N log lines in
// a ring buffer. The standard library's `log` package writes whole lines
// at a time (each Printf produces one Write call ending in '\n'), so we
// can treat each Write as a line — split on the trailing newline and
// store. Used as a sink for log.SetOutput so the diagnostics page can
// show recent log activity alongside the gate-drop ring.
//
// Concurrency: log.Print* calls Write from arbitrary goroutines; the
// mutex makes append + Snapshot safe.
type LogBuffer struct {
	cap   int
	mu    sync.Mutex
	lines []LogLine
}

// LogLine is one timestamped log entry.
type LogLine struct {
	Time    time.Time
	Message string
}

// NewLogBuffer creates an empty ring buffer with the given capacity.
// Older entries fall off the front as new ones come in.
func NewLogBuffer(cap int) *LogBuffer {
	if cap <= 0 {
		cap = 200
	}
	return &LogBuffer{cap: cap, lines: make([]LogLine, 0, cap)}
}

// Write implements io.Writer. The standard log package emits each
// formatted record as a single Write ending in '\n'; we split on that
// so multi-line log entries (which include the trailing newline from
// log.Print* plus our own embedded newlines) each get their own entry.
// Leading log-package timestamp (the "YYYY/MM/DD HH:MM:SS " prefix) is
// stripped because we already keep our own Time on each LogLine.
func (b *LogBuffer) Write(p []byte) (int, error) {
	n := len(p)
	// Each Write from the log package corresponds to one record; split
	// on newline anyway in case a caller passes a multi-line buffer.
	for _, raw := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		if raw == "" {
			continue
		}
		// Strip the "2026/01/01 12:34:56 " prefix the std log adds when
		// it's the default flags. Just look for the second space within
		// the first 20 chars.
		msg := raw
		if len(raw) > 20 && raw[4] == '/' && raw[7] == '/' && raw[10] == ' ' && raw[13] == ':' && raw[16] == ':' {
			msg = strings.TrimSpace(raw[19:])
		}
		b.append(LogLine{Time: time.Now(), Message: msg})
	}
	return n, nil
}

func (b *LogBuffer) append(l LogLine) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.lines) < b.cap {
		b.lines = append(b.lines, l)
		return
	}
	// Full — shift left by one and append at the end. Cheap at 200 cap.
	copy(b.lines, b.lines[1:])
	b.lines[len(b.lines)-1] = l
}

// Snapshot returns a copy of the buffered lines, oldest first.
func (b *LogBuffer) Snapshot() []LogLine {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]LogLine, len(b.lines))
	copy(out, b.lines)
	return out
}

// Tee returns an io.Writer that writes to both this LogBuffer and the
// given target (typically os.Stderr) so journalctl keeps getting the
// log stream while the UI also has a copy.
func (b *LogBuffer) Tee(other io.Writer) io.Writer {
	return io.MultiWriter(other, b)
}
