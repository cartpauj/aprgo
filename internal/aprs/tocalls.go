package aprs

import (
	_ "embed"
	"encoding/json"
	"sort"
	"strings"
)

// Tocall identification — derived from the canonical aprs-deviceid registry
// at github.com/aprsorg/aprs-deviceid (CC BY-SA 2.0). The registry is the
// machine-readable list of tocall destination addresses → device/software
// identities. We slim it down to {tocall, vendor, model, os, class} at
// build-time (see internal/aprs/data/tocalls.json) and embed the result
// here so lookups have no runtime dependency on network or filesystem.
//
// Tocalls can be exact (e.g. "APRGO") or contain '?' wildcards (e.g.
// "APDW1?" matches Direwolf 1.x). Wildcards are always 6-char patterns;
// exact entries can be 4, 5, or 6 chars.

//go:embed data/tocalls.json
var tocallsJSON []byte

// DeviceID describes the vendor / model behind a tocall destination. Empty
// strings mean the registry didn't include that field for the entry.
type DeviceID struct {
	Tocall string `json:"tocall"`
	Vendor string `json:"vendor,omitempty"`
	Model  string `json:"model,omitempty"`
	OS     string `json:"os,omitempty"`
	Class  string `json:"class,omitempty"`
}

// Display returns a short, human-readable identity string suitable for chip
// rendering. Format: "Vendor Model" if both exist, "Model" otherwise.
// Falls back to the raw tocall if neither is present.
func (d DeviceID) Display() string {
	switch {
	case d.Vendor != "" && d.Model != "":
		return d.Vendor + " " + d.Model
	case d.Model != "":
		return d.Model
	case d.Vendor != "":
		return d.Vendor
	}
	return d.Tocall
}

var (
	tocallExact    map[string]DeviceID
	tocallWildcard []DeviceID // wildcards: sorted by length asc then specificity asc (fewer ?s first)
)

func init() {
	var raw struct {
		Tocalls []DeviceID `json:"tocalls"`
	}
	if err := json.Unmarshal(tocallsJSON, &raw); err != nil {
		// Embedded file is generated locally — a parse error here means
		// the embed is corrupt, which is a build-time problem. Leave the
		// maps empty; LookupDevice will just return zero values.
		return
	}
	tocallExact = make(map[string]DeviceID, len(raw.Tocalls))
	for _, e := range raw.Tocalls {
		if strings.ContainsRune(e.Tocall, '?') {
			tocallWildcard = append(tocallWildcard, e)
		} else {
			tocallExact[e.Tocall] = e
		}
	}
	// Sort wildcards: shorter pattern length first (so 5-char wildcards
	// like APAGW? match before they're considered against a 6-char dest),
	// then by number of literal characters descending (most-specific first).
	sort.SliceStable(tocallWildcard, func(i, j int) bool {
		li, lj := len(tocallWildcard[i].Tocall), len(tocallWildcard[j].Tocall)
		if li != lj {
			return li < lj
		}
		return wildcardSpecificity(tocallWildcard[i].Tocall) > wildcardSpecificity(tocallWildcard[j].Tocall)
	})
}

func wildcardSpecificity(p string) int {
	return len(p) - strings.Count(p, "?")
}

// LookupDevice resolves an AX.25 destination address into a DeviceID using
// the aprs-deviceid registry. Returns the zero-value DeviceID with Tocall
// set to the input when no registry entry matches — callers can check
// Display() == dest to detect "unknown."
//
// Exact matches always win. Wildcard matches are tried for entries whose
// length is == len(dest); within that set, more-specific patterns
// (fewer '?'s) are checked first. The dest is upper-cased before lookup
// since AX.25 addresses are uppercase by spec.
func LookupDevice(dest string) DeviceID {
	dest = strings.ToUpper(strings.TrimSpace(dest))
	if dest == "" {
		return DeviceID{}
	}
	// Strip SSID — destinations in the registry are bare tocalls.
	if dash := strings.IndexByte(dest, '-'); dash >= 0 {
		dest = dest[:dash]
	}
	if d, ok := tocallExact[dest]; ok {
		return d
	}
	for _, w := range tocallWildcard {
		if len(w.Tocall) != len(dest) {
			continue
		}
		if wildcardMatch(w.Tocall, dest) {
			out := w
			out.Tocall = dest // report the actual dest, not the pattern
			return out
		}
	}
	return DeviceID{Tocall: dest}
}

// wildcardMatch compares pattern (containing '?' wildcards) against dest.
// Both must be the same length; caller guarantees this.
func wildcardMatch(pattern, dest string) bool {
	for i := 0; i < len(pattern); i++ {
		if pattern[i] == '?' {
			continue
		}
		if pattern[i] != dest[i] {
			return false
		}
	}
	return true
}
