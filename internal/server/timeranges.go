package server

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// TimeRange is one entry in the canonical lookback-window list used by the
// Map and Stations pages. Value is the wire/query form (e.g. "24h"), Label
// is what the dropdown shows, Dur is the resolved duration.
type TimeRange struct {
	Value string
	Label string
	Dur   time.Duration
}

// canonicalTimeRanges is the single source of truth for lookback windows
// shown anywhere in the UI. Add an entry here and it appears wherever the
// time-range list is rendered.
var canonicalTimeRanges = []TimeRange{
	{"30m", "30 minutes", 30 * time.Minute},
	{"1h", "1 hour", 1 * time.Hour},
	{"3h", "3 hours", 3 * time.Hour},
	{"6h", "6 hours", 6 * time.Hour},
	{"12h", "12 hours", 12 * time.Hour},
	{"24h", "24 hours", 24 * time.Hour},
	{"3d", "3 days", 3 * 24 * time.Hour},
	{"7d", "7 days", 7 * 24 * time.Hour},
	{"30d", "30 days", 30 * 24 * time.Hour},
}

// FilteredTimeRanges returns the canonical list capped at the operator's
// configured retention. Entries strictly larger than retention are dropped.
// If the largest remaining entry is shorter than the actual retention, a
// synthesized "N days"/"N hours" entry is appended so the operator can
// still query up to the data they have. retentionDays<=0 means "forever"
// — no filtering applied.
func FilteredTimeRanges(retentionDays int) []TimeRange {
	if retentionDays <= 0 {
		out := make([]TimeRange, len(canonicalTimeRanges))
		copy(out, canonicalTimeRanges)
		return out
	}
	maxDur := time.Duration(retentionDays) * 24 * time.Hour
	out := make([]TimeRange, 0, len(canonicalTimeRanges))
	for _, r := range canonicalTimeRanges {
		if r.Dur <= maxDur {
			out = append(out, r)
		}
	}
	// If retention falls between two canonical entries, the largest filtered
	// entry will be strictly less than maxDur — append a synthetic entry
	// for the exact retention so the operator can pick the full window.
	if len(out) == 0 || out[len(out)-1].Dur < maxDur {
		label := fmt.Sprintf("%d days", retentionDays)
		if retentionDays == 1 {
			label = "1 day"
		}
		out = append(out, TimeRange{
			Value: fmt.Sprintf("%dd", retentionDays),
			Label: label,
			Dur:   maxDur,
		})
	}
	return out
}

// ParseWindow turns a "30m" / "1h" / "3d" / "24h" string into a duration.
// Returns (0, false) if the string isn't well-formed. Accepts integer
// quantity + single-letter unit only — no "1h30m" composites.
func ParseWindow(s string) (time.Duration, bool) {
	s = strings.TrimSpace(s)
	if len(s) < 2 {
		return 0, false
	}
	unit := s[len(s)-1]
	n, err := strconv.Atoi(s[:len(s)-1])
	if err != nil || n <= 0 {
		return 0, false
	}
	switch unit {
	case 'm':
		return time.Duration(n) * time.Minute, true
	case 'h':
		return time.Duration(n) * time.Hour, true
	case 'd':
		return time.Duration(n) * 24 * time.Hour, true
	}
	return 0, false
}

// resolveWindow looks up the duration for a query-string window value,
// honoring the operator's retention. If the requested window is not in
// the filtered set, falls back to fallbackValue (also looked up in the
// filtered set; if that isn't present either, the first entry is used).
func resolveWindow(reqValue, fallbackValue string, retentionDays int) (TimeRange, []TimeRange) {
	opts := FilteredTimeRanges(retentionDays)
	for _, o := range opts {
		if o.Value == reqValue {
			return o, opts
		}
	}
	for _, o := range opts {
		if o.Value == fallbackValue {
			return o, opts
		}
	}
	if len(opts) > 0 {
		return opts[0], opts
	}
	return TimeRange{Value: "1h", Label: "1 hour", Dur: time.Hour}, opts
}
