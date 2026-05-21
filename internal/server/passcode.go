package server

import (
	"strings"
)

// aprsISPasscode computes the well-known APRS-IS passcode hash for a callsign.
// The algorithm is a 16-bit XOR hash of the uppercase base callsign (SSID
// stripped); the high bit is masked off, yielding a 15-bit positive integer.
// It is independently published in every iGate implementation (Xastir, aprx,
// Direwolf, YAAC, APRSdroid) — not a secret. Returns 0 for empty input.
func aprsISPasscode(call string) int {
	call = strings.ToUpper(strings.TrimSpace(call))
	if i := strings.IndexByte(call, '-'); i >= 0 {
		call = call[:i]
	}
	if call == "" {
		return 0
	}
	hash := 0x73e2
	for i := 0; i < len(call); i += 2 {
		hash ^= int(call[i]) << 8
		if i+1 < len(call) {
			hash ^= int(call[i+1])
		}
	}
	return hash & 0x7fff
}

// aprsISPasscodeMatches reports whether the supplied passcode string is the
// expected hash for the callsign. Returns true for the special string "-1"
// (the APRS-IS sentinel for "I am receive-only / cannot verify"). Empty
// passcode returns false.
func aprsISPasscodeMatches(callsign, passcode string) bool {
	passcode = strings.TrimSpace(passcode)
	if passcode == "" {
		return false
	}
	if passcode == "-1" {
		return true
	}
	want := aprsISPasscode(callsign)
	return passcode == itoaInt(want)
}

// itoaInt avoids importing strconv just for this file.
func itoaInt(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [12]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
