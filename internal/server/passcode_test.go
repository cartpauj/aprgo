package server

import "testing"

func TestAPRSISPasscode(t *testing.T) {
	// Reference values cross-checked against aprx (open source) and the
	// well-known "N0CALL → 13023" datum every iGate operator has seen.
	cases := []struct {
		call string
		want int
	}{
		{"N0CALL", 13023},
		{"n0call", 13023},    // case-insensitive
		{"N0CALL-10", 13023}, // SSID stripped
	}
	for _, c := range cases {
		got := aprsISPasscode(c.call)
		if got != c.want {
			t.Errorf("aprsISPasscode(%q) = %d, want %d", c.call, got, c.want)
		}
	}
}

func TestAPRSISPasscodeMatches(t *testing.T) {
	if !aprsISPasscodeMatches("N0CALL", "13023") {
		t.Error("N0CALL/13023 should match")
	}
	if !aprsISPasscodeMatches("N0CALL-10", "-1") {
		t.Error("-1 sentinel should always match")
	}
	if aprsISPasscodeMatches("N0CALL", "99999") {
		t.Error("wrong passcode should not match")
	}
	if aprsISPasscodeMatches("N0CALL", "") {
		t.Error("empty passcode should not match")
	}
}
