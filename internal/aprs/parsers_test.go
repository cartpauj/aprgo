package aprs

import (
	"strings"
	"testing"
)

// Weather parser: real-shape positional weather report sample
// _DDD/SSSgGGGtTTTrRRRpPPPPPPPhHHbBBBBB
func TestParseWeatherFull(t *testing.T) {
	in := "272/000g006t069r010p030P020h61b10153 KP4 Wx station"
	w, stripped := parseWeather(in)
	if w == nil {
		t.Fatalf("expected weather decode, got nil")
	}
	if !w.WindDirSet || w.WindDirDeg != 272 {
		t.Errorf("wind dir: got %d set=%v", w.WindDirDeg, w.WindDirSet)
	}
	if !w.WindSpeedSet || w.WindSpeedMPH != 0 {
		t.Errorf("wind spd: got %d set=%v", w.WindSpeedMPH, w.WindSpeedSet)
	}
	if !w.WindGustSet || w.WindGustMPH != 6 {
		t.Errorf("gust: got %d set=%v", w.WindGustMPH, w.WindGustSet)
	}
	if !w.TempSet || w.TempF != 69 {
		t.Errorf("temp: got %d set=%v", w.TempF, w.TempSet)
	}
	if !w.Rain1hSet || w.Rain1hHundIn != 10 {
		t.Errorf("rain1h: got %d", w.Rain1hHundIn)
	}
	if !w.HumiditySet || w.HumidityPct != 61 {
		t.Errorf("humidity: got %d", w.HumidityPct)
	}
	if !w.PressureSet || w.PressureTenthMb != 10153 {
		t.Errorf("pressure: got %d", w.PressureTenthMb)
	}
	if !strings.Contains(stripped, "KP4 Wx station") {
		t.Errorf("expected residual comment to retain free-text: %q", stripped)
	}
	if strings.Contains(stripped, "t069") || strings.Contains(stripped, "b10153") {
		t.Errorf("expected weather fields stripped: %q", stripped)
	}
}

// Humidity 00 means 100%.
func TestParseWeatherHumidity00(t *testing.T) {
	w, _ := parseWeather("g005t070h00b10000")
	if w == nil || !w.HumiditySet || w.HumidityPct != 100 {
		t.Fatalf("00 humidity should map to 100%%, got %+v", w)
	}
}

// No weather data should yield nil.
func TestParseWeatherNone(t *testing.T) {
	if w, _ := parseWeather("Just a regular comment"); w != nil {
		t.Fatalf("expected nil weather, got %+v", w)
	}
}

// Don't be fooled by a single "g123" appearing in an unrelated comment.
func TestParseWeatherSingleFieldRejected(t *testing.T) {
	if w, _ := parseWeather("Repeater g123 chat"); w != nil {
		t.Fatalf("single weather-looking field should not trigger: %+v", w)
	}
}

// PHG: PHG5132 → 25W (5²), 20ft (10·2¹), 3dB, 90° (E)
func TestParsePHG(t *testing.T) {
	in := "Fill-in iGate PHG5132 home"
	p, stripped := parsePHG(in)
	if p == nil {
		t.Fatalf("expected PHG, got nil")
	}
	if p.PowerW != 25 {
		t.Errorf("power: got %d", p.PowerW)
	}
	if p.HeightFt != 20 {
		t.Errorf("height: got %d", p.HeightFt)
	}
	if p.GainDB != 3 {
		t.Errorf("gain: got %d", p.GainDB)
	}
	if p.Omni || p.DirDeg != 90 {
		t.Errorf("dir: got %d omni=%v", p.DirDeg, p.Omni)
	}
	if p.RangeMiles < 1 || p.RangeMiles > 200 {
		t.Errorf("range plausibility check failed: %v", p.RangeMiles)
	}
	if strings.Contains(stripped, "PHG5132") {
		t.Errorf("expected PHG stripped: %q", stripped)
	}
}

func TestParsePHGOmni(t *testing.T) {
	p, _ := parsePHG("PHG4360")
	if p == nil || !p.Omni || p.DirDeg != 0 {
		t.Fatalf("expected omni, got %+v", p)
	}
}

func TestParseRNG(t *testing.T) {
	r, stripped := parseRNG("RNG0050 mountaintop digi")
	if r == nil || r.Miles != 50 {
		t.Fatalf("expected RNG=50, got %+v", r)
	}
	if strings.Contains(stripped, "RNG") {
		t.Errorf("expected RNG stripped: %q", stripped)
	}
}

// Tocall lookup — exact match against a known-registered tocall.
// APRDOS is the original Bob Bruninga DOS tracker — has been registered
// since the dawn of APRS, safe to assert.
func TestLookupDeviceExact(t *testing.T) {
	d := LookupDevice("APRS")
	if d.Tocall == "" {
		t.Errorf("expected lookup to return something for APRS, got %+v", d)
	}
}

func TestLookupDeviceWildcard(t *testing.T) {
	// Direwolf 1.7 — should match APDW1?
	d := LookupDevice("APDW17")
	if d.Model == "" {
		t.Errorf("expected model for APDW17 via wildcard, got %+v", d)
	}
}

func TestLookupDeviceUnknown(t *testing.T) {
	d := LookupDevice("XXXXXX")
	if d.Model != "" || d.Vendor != "" {
		t.Errorf("expected unknown for XXXXXX, got %+v", d)
	}
	if d.Tocall != "XXXXXX" {
		t.Errorf("expected tocall echoed back, got %q", d.Tocall)
	}
}

func TestLookupDeviceStripsSSID(t *testing.T) {
	// SSID on destination is unusual but tolerable; the lookup should
	// strip it before consulting the registry.
	d := LookupDevice("APMI04-1")
	if d.Vendor == "" {
		t.Errorf("expected APMI04 lookup to succeed even with -1 SSID")
	}
}

// Path parsing — used hops, q-construct, and counts.
func TestParsePathDirect(t *testing.T) {
	p := ParsePath("WIDE1-1,WIDE2-1")
	if p.DigiCapable != 2 || p.DigiCount != 0 {
		t.Errorf("unused path: capable=%d used=%d", p.DigiCapable, p.DigiCount)
	}
	if p.HopSummary() != "Direct (path unused)" {
		t.Errorf("summary: %q", p.HopSummary())
	}
}

func TestParsePathOneDigi(t *testing.T) {
	p := ParsePath("WIDE1*,WIDE2-1")
	if p.DigiCount != 1 || p.DigiCapable != 2 {
		t.Errorf("one digi used: capable=%d used=%d", p.DigiCapable, p.DigiCount)
	}
	if !p.Hops[0].Used || p.Hops[1].Used {
		t.Errorf("hop used flags wrong: %+v", p.Hops)
	}
}

func TestParsePathQConstruct(t *testing.T) {
	p := ParsePath("WIDE1*,WIDE2-1*,qAR,N0CALL-10")
	if p.QConstruct != "qAR" {
		t.Errorf("q-construct: %q", p.QConstruct)
	}
	if p.IGateCall != "N0CALL-10" {
		t.Errorf("igate: %q", p.IGateCall)
	}
	if p.DigiCount != 2 {
		t.Errorf("digi count: %d", p.DigiCount)
	}
	// The qAR + iGate hops should be flagged as q-construct.
	for _, h := range p.Hops[2:] {
		if !h.IsQConstruct {
			t.Errorf("expected IsQConstruct on tail hops: %+v", h)
		}
	}
}

func TestParsePathEmpty(t *testing.T) {
	p := ParsePath("")
	if p.DigiCapable != 0 || p.HopSummary() != "Direct" {
		t.Errorf("empty path: %+v summary=%q", p, p.HopSummary())
	}
}
