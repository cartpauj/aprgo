package gate

import (
	"strings"
	"testing"

	"aprgo/internal/aprs"
	"aprgo/internal/ax25"
	"aprgo/internal/state"
)

// helper: build a parsed RF packet with the given source and path
func rfPacket(src, dest string, path []string, info string) aprs.Packet {
	raw, _ := ax25.EncodeUIFrame(src, dest, path, []byte(info))
	frame, _ := ax25.FromAX25(raw, ax25.SrcRF, "rf")
	return aprs.Parse(frame)
}

// extract the path of the re-encoded SendRF frame
func decodedPath(t *testing.T, raw []byte) []string {
	t.Helper()
	fr, err := ax25.FromAX25(raw, ax25.SrcRF, "rf")
	if err != nil {
		t.Fatalf("decode roundtrip: %v", err)
	}
	return fr.Path
}

func TestParseWIDE(t *testing.T) {
	cases := []struct {
		in        string
		wantN, wn int
		wantOK    bool
	}{
		{"WIDE1-1", 1, 1, true},
		{"WIDE2-2", 2, 2, true},
		{"WIDE2-1", 2, 1, true},
		{"WIDE2-0", 2, 0, true},
		{"wide1-1", 1, 1, true}, // case insensitive prefix
		{"WIDE1-1*", 0, 0, false}, // used-bit variants aren't parsed
		{"WIDE", 0, 0, false},
		{"WIDE-1", 0, 0, false},
		{"WIDE1-", 0, 0, false},
		{"WIDEN-N", 0, 0, false},
		{"N0CALL-9", 0, 0, false},
		{"WIDE8-1", 0, 0, false}, // n out of range
	}
	for _, c := range cases {
		n, N, ok := parseWIDE(c.in)
		if ok != c.wantOK || n != c.wantN || N != c.wn {
			t.Errorf("parseWIDE(%q) = (%d,%d,%v), want (%d,%d,%v)",
				c.in, n, N, ok, c.wantN, c.wn, c.wantOK)
		}
	}
}

func TestDigipeatWIDE1Fill(t *testing.T) {
	s := state.State{Callsign: "N0CALL-10", TXEnable: true, DigipeatWIDE1: true}
	pkt := rfPacket("N0CALL-9", "APRGO", []string{"WIDE1-1"}, "=test")
	actions := decideFromRF(pkt, s)
	got := findSendRF(actions)
	if got == nil {
		t.Fatal("expected SendRF for WIDE1-1, got none")
	}
	path := decodedPath(t, got.RFRaw)
	if len(path) != 1 || path[0] != "N0CALL-10*" {
		t.Fatalf("WIDE1-1 should be replaced with MYCALL*; got %v", path)
	}
}

func TestDigipeatWIDE2Decrement(t *testing.T) {
	s := state.State{Callsign: "N0CALL-10", TXEnable: true, DigipeatWIDE2: true}
	pkt := rfPacket("N0CALL-9", "APRGO", []string{"WIDE2-2"}, "=test")
	got := findSendRF(decideFromRF(pkt, s))
	if got == nil {
		t.Fatal("expected SendRF for WIDE2-2 in full-digi mode")
	}
	path := decodedPath(t, got.RFRaw)
	if len(path) != 2 || path[0] != "N0CALL-10*" || path[1] != "WIDE2-1" {
		t.Fatalf("WIDE2-2 should become MYCALL*,WIDE2-1; got %v", path)
	}
}

func TestDigipeatWIDE2LastHop(t *testing.T) {
	s := state.State{Callsign: "N0CALL-10", TXEnable: true, DigipeatWIDE2: true}
	pkt := rfPacket("N0CALL-9", "APRGO", []string{"WIDE2-1"}, "=test")
	got := findSendRF(decideFromRF(pkt, s))
	if got == nil {
		t.Fatal("expected SendRF for WIDE2-1")
	}
	path := decodedPath(t, got.RFRaw)
	if len(path) != 1 || path[0] != "N0CALL-10*" {
		t.Fatalf("WIDE2-1 (last hop) should become MYCALL*; got %v", path)
	}
}

func TestFillinModeRefusesWIDE2(t *testing.T) {
	s := state.State{Callsign: "N0CALL-10", TXEnable: true,
		DigipeatWIDE1: true, DigipeatWIDE2: false}
	pkt := rfPacket("N0CALL-9", "APRGO", []string{"WIDE2-2"}, "=test")
	if findSendRF(decideFromRF(pkt, s)) != nil {
		t.Fatal("fill-in mode should NOT handle WIDE2-2")
	}
}

func TestFullDigiHandlesBoth(t *testing.T) {
	s := state.State{Callsign: "N0CALL-10", TXEnable: true,
		DigipeatWIDE1: true, DigipeatWIDE2: true}
	for _, p := range []string{"WIDE1-1", "WIDE2-2", "WIDE2-1"} {
		pkt := rfPacket("N0CALL-9", "APRGO", []string{p}, "=test")
		if findSendRF(decideFromRF(pkt, s)) == nil {
			t.Errorf("full-digi should handle %s", p)
		}
	}
}

func TestRefuseExcessiveN(t *testing.T) {
	s := state.State{Callsign: "N0CALL-10", TXEnable: true, DigipeatWIDE2: true}
	for _, p := range []string{"WIDE2-3", "WIDE2-7", "WIDE3-3"} {
		pkt := rfPacket("N0CALL-9", "APRGO", []string{p}, "=test")
		if findSendRF(decideFromRF(pkt, s)) != nil {
			t.Errorf("should refuse abusive %s", p)
		}
	}
}

func TestSkipIfMyCallAlreadyInPath(t *testing.T) {
	s := state.State{Callsign: "N0CALL-10", TXEnable: true, DigipeatWIDE1: true}
	// "We're already in the path (used)"
	pkt := rfPacket("N0CALL-9", "APRGO", []string{"N0CALL-10*", "WIDE1-1"}, "=test")
	if findSendRF(decideFromRF(pkt, s)) != nil {
		t.Fatal("should not re-digipeat when MYCALL is already in path")
	}
}

func TestSkipUsedHops(t *testing.T) {
	// First unused hop after a used WIDE1-1* is what we'd handle.
	s := state.State{Callsign: "N0CALL-10", TXEnable: true, DigipeatWIDE2: true}
	pkt := rfPacket("N0CALL-9", "APRGO", []string{"OTHER-1*", "WIDE2-1"}, "=test")
	got := findSendRF(decideFromRF(pkt, s))
	if got == nil {
		t.Fatal("expected SendRF: should handle WIDE2-1 after a prior used hop")
	}
	path := decodedPath(t, got.RFRaw)
	if len(path) != 2 || path[0] != "OTHER-1*" || path[1] != "N0CALL-10*" {
		t.Fatalf("expected [OTHER-1*, N0CALL-10*]; got %v", path)
	}
}

func TestRefuseOwnBeacon(t *testing.T) {
	s := state.State{Callsign: "N0CALL-10", TXEnable: true, DigipeatWIDE2: true}
	pkt := rfPacket("N0CALL-10", "APRGO", []string{"WIDE2-2"}, "=test")
	if findSendRF(decideFromRF(pkt, s)) != nil {
		t.Fatal("should refuse to digipeat own beacon")
	}
}

func TestRefuseWhenTXDisabled(t *testing.T) {
	s := state.State{Callsign: "N0CALL-10", TXEnable: false,
		DigipeatWIDE1: true, DigipeatWIDE2: true}
	pkt := rfPacket("N0CALL-9", "APRGO", []string{"WIDE1-1"}, "=test")
	if findSendRF(decideFromRF(pkt, s)) != nil {
		t.Fatal("TXEnable=false must suppress digipeat")
	}
}

func TestPathLengthCap(t *testing.T) {
	s := state.State{Callsign: "N0CALL-10", TXEnable: true, DigipeatWIDE2: true}
	// 8 hops already — handling WIDE2-2 would require inserting MYCALL*
	// pushing the path to 9, which exceeds the AX.25 cap. Skip.
	path := []string{"A*", "B*", "C*", "D*", "E*", "F*", "G*", "WIDE2-2"}
	pkt := rfPacket("N0CALL-9", "APRGO", path, "=test")
	if findSendRF(decideFromRF(pkt, s)) != nil {
		t.Fatal("should refuse to grow path beyond 8 hops")
	}
}

func TestViscousFlagOnWIDE1(t *testing.T) {
	// ViscousDelay on → WIDE1-1 fill-in gets Viscous=true
	s := state.State{Callsign: "N0CALL-10", TXEnable: true,
		DigipeatWIDE1: true, ViscousDelay: true}
	pkt := rfPacket("N0CALL-9", "APRGO", []string{"WIDE1-1"}, "=test")
	got := findSendRF(decideFromRF(pkt, s))
	if got == nil || !got.Viscous {
		t.Fatalf("WIDE1-1 with ViscousDelay should set Viscous=true; got %+v", got)
	}
}

func TestViscousFlagOffWhenDisabled(t *testing.T) {
	s := state.State{Callsign: "N0CALL-10", TXEnable: true,
		DigipeatWIDE1: true, ViscousDelay: false}
	pkt := rfPacket("N0CALL-9", "APRGO", []string{"WIDE1-1"}, "=test")
	got := findSendRF(decideFromRF(pkt, s))
	if got == nil || got.Viscous {
		t.Fatalf("ViscousDelay=false → Viscous must stay false; got %+v", got)
	}
}

func TestViscousNeverOnWIDE2(t *testing.T) {
	// WIDE2-N is authoritative; should never be viscous-delayed even with
	// the flag on.
	s := state.State{Callsign: "N0CALL-10", TXEnable: true,
		DigipeatWIDE2: true, ViscousDelay: true}
	pkt := rfPacket("N0CALL-9", "APRGO", []string{"WIDE2-2"}, "=test")
	got := findSendRF(decideFromRF(pkt, s))
	if got == nil || got.Viscous {
		t.Fatalf("WIDE2-N must not be viscous; got %+v", got)
	}
}

// findSendRF returns the first SendRF Action in a slice, or nil.
func findSendRF(actions []Action) *Action {
	for i := range actions {
		if actions[i].Kind == SendRF {
			return &actions[i]
		}
	}
	return nil
}

// Preemptive: MYCALL explicitly listed in path (not first unused).
// With flag off, we ignore it (current behavior).
func TestPreemptiveDisabledByDefault(t *testing.T) {
	s := state.State{Callsign: "APRGO-1", TXEnable: true, DigipeatWIDE2: true}
	pkt := rfPacket("N0CALL-9", "APRGO", []string{"WIDE2-1", "APRGO-1", "WIDE2-1"}, "=hi")
	got := findSendRF(decideFromRF(pkt, s))
	if got != nil {
		t.Fatalf("preempt off: expected no SendRF, got path %v", decodedPath(t, got.RFRaw))
	}
}

// Preemptive enabled: scan ahead, MARK-mode the prior unused hops.
func TestPreemptiveMarksPriorHops(t *testing.T) {
	s := state.State{Callsign: "APRGO-1", TXEnable: true, PreemptiveDigipeat: true}
	pkt := rfPacket("N0CALL-9", "APRGO", []string{"WIDE2-1", "APRGO-1", "WIDE2-1"}, "=hi")
	got := findSendRF(decideFromRF(pkt, s))
	if got == nil {
		t.Fatal("preempt on: expected SendRF, got none")
	}
	path := decodedPath(t, got.RFRaw)
	if strings.Join(path, ",") != "WIDE2-1*,APRGO-1*,WIDE2-1" {
		t.Fatalf("preempt MARK: got %v, want WIDE2-1*,APRGO-1*,WIDE2-1", path)
	}
}

// Preemptive must never trigger on generic WIDEn-N — that would break
// the normal flood semantics. With ONLY PreemptiveDigipeat on (no WIDE
// flags), a pure-WIDE path should be ignored.
func TestPreemptiveDoesNotMatchGenericWIDE(t *testing.T) {
	s := state.State{Callsign: "APRGO-1", TXEnable: true, PreemptiveDigipeat: true}
	pkt := rfPacket("N0CALL-9", "APRGO", []string{"WIDE2-2", "WIDE2-1"}, "=hi")
	got := findSendRF(decideFromRF(pkt, s))
	if got != nil {
		t.Fatalf("preempt should not fire on pure WIDE path; got %v", decodedPath(t, got.RFRaw))
	}
}

// Preemptive: MYCALL is the next unused hop (the simple case). With
// preempt on we should handle it directly — no path before us to mark.
func TestPreemptiveNextHopExplicit(t *testing.T) {
	s := state.State{Callsign: "APRGO-1", TXEnable: true, PreemptiveDigipeat: true}
	pkt := rfPacket("N0CALL-9", "APRGO", []string{"APRGO-1", "WIDE2-1"}, "=hi")
	got := findSendRF(decideFromRF(pkt, s))
	if got == nil {
		t.Fatal("expected SendRF for MYCALL-as-next-hop with preempt on")
	}
	path := decodedPath(t, got.RFRaw)
	if strings.Join(path, ",") != "APRGO-1*,WIDE2-1" {
		t.Fatalf("got %v, want APRGO-1*,WIDE2-1", path)
	}
}

// Preemptive: if we've already digipeated (MYCALL is in path with *),
// don't act again — even with preempt on.
func TestPreemptiveSkipsAlreadyHandled(t *testing.T) {
	s := state.State{Callsign: "APRGO-1", TXEnable: true, PreemptiveDigipeat: true, DigipeatWIDE2: true}
	pkt := rfPacket("N0CALL-9", "APRGO", []string{"APRGO-1*", "WIDE2-1"}, "=hi")
	got := findSendRF(decideFromRF(pkt, s))
	if got != nil {
		t.Fatalf("expected no SendRF when already handled; got %v", decodedPath(t, got.RFRaw))
	}
}

// Sanity: ensure the example trace from APRS101 §13 still works.
// `N0CALL>APRS,WIDE1-1,WIDE2-1` → fill-in then full-digi.
func TestExampleFillThenFull(t *testing.T) {
	// Stage 1: fill-in handles WIDE1-1
	fill := state.State{Callsign: "FILL-1", TXEnable: true, DigipeatWIDE1: true}
	stage1 := rfPacket("N0CALL", "APRS", []string{"WIDE1-1", "WIDE2-1"}, "=hi")
	a1 := findSendRF(decideFromRF(stage1, fill))
	if a1 == nil {
		t.Fatal("stage1: fill-in should handle WIDE1-1")
	}
	p1 := decodedPath(t, a1.RFRaw)
	if strings.Join(p1, ",") != "FILL-1*,WIDE2-1" {
		t.Fatalf("stage1 path: got %v, want FILL-1*,WIDE2-1", p1)
	}
	// Stage 2: full-digi handles the now-current WIDE2-1
	digi := state.State{Callsign: "DIGI-1", TXEnable: true, DigipeatWIDE2: true}
	stage2 := rfPacket("N0CALL", "APRS", p1, "=hi")
	a2 := findSendRF(decideFromRF(stage2, digi))
	if a2 == nil {
		t.Fatal("stage2: full-digi should handle WIDE2-1")
	}
	p2 := decodedPath(t, a2.RFRaw)
	if strings.Join(p2, ",") != "FILL-1*,DIGI-1*" {
		t.Fatalf("stage2 path: got %v, want FILL-1*,DIGI-1*", p2)
	}
}
