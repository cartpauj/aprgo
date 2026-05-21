package aprs

import "testing"

// APRS telemetry-configuration messages per APRS101 §13.4. Format:
//
//   :CALLSIGN-N:PARM.P1,P2,P3,P4,P5,B1,B2,B3,B4,B5,B6,B7,B8
//   :CALLSIGN-N:UNIT.U1,U2,U3,U4,U5,L1,L2,L3,L4,L5,L6,L7,L8
//   :CALLSIGN-N:EQNS.A1,B1,C1,A2,B2,C2,A3,B3,C3,A4,B4,C4,A5,B5,C5
//   :CALLSIGN-N:BITS.XXXXXXXX,Project Title (up to 23 chars)
//
// 5 analog channels (values = A*X² + B*X + C) + 8 digital bits.
//
// These messages addressed to the station they describe; the operator
// transmits them periodically so receivers can label their otherwise-
// generic T# data. Real example captured from KK7LLM-13:
//   :KK7LLM-13:PARM.Battery,Temp

func TestTelemConfigPARM(t *testing.T) {
	d := Decode(":KK7LLM-13:PARM.Battery,Temp", "APRS")
	if d.TelemConfig == nil {
		t.Fatalf("expected TelemConfig set, got nil")
	}
	if d.TelemConfig.Kind != "parm" {
		t.Errorf("Kind: got %q want %q", d.TelemConfig.Kind, "parm")
	}
	if d.TelemConfig.ParamNames[0] != "Battery" {
		t.Errorf("analog[0]: got %q want %q", d.TelemConfig.ParamNames[0], "Battery")
	}
	if d.TelemConfig.ParamNames[1] != "Temp" {
		t.Errorf("analog[1]: got %q want %q", d.TelemConfig.ParamNames[1], "Temp")
	}
	if d.IsMessage {
		t.Errorf("telemetry config should not be flagged as a regular message")
	}
}

func TestTelemConfigPARMFull(t *testing.T) {
	// All 13 labels.
	d := Decode(":N0CALL   :PARM.A1,A2,A3,A4,A5,B1,B2,B3,B4,B5,B6,B7,B8", "APRS")
	if d.TelemConfig == nil {
		t.Fatalf("expected TelemConfig")
	}
	want := []string{"A1", "A2", "A3", "A4", "A5", "B1", "B2", "B3", "B4", "B5", "B6", "B7", "B8"}
	for i, w := range want {
		if d.TelemConfig.ParamNames[i] != w {
			t.Errorf("ParamNames[%d]: got %q want %q", i, d.TelemConfig.ParamNames[i], w)
		}
	}
}

func TestTelemConfigUNIT(t *testing.T) {
	d := Decode(":KK7LLM-13:UNIT.Volts,Deg.F", "APRS")
	if d.TelemConfig == nil {
		t.Fatalf("expected TelemConfig")
	}
	if d.TelemConfig.Kind != "unit" {
		t.Errorf("Kind: got %q want %q", d.TelemConfig.Kind, "unit")
	}
	if d.TelemConfig.UnitNames[0] != "Volts" {
		t.Errorf("UnitNames[0]: got %q want %q", d.TelemConfig.UnitNames[0], "Volts")
	}
	if d.TelemConfig.UnitNames[1] != "Deg.F" {
		t.Errorf("UnitNames[1]: got %q want %q", d.TelemConfig.UnitNames[1], "Deg.F")
	}
}

func TestTelemConfigEQNS(t *testing.T) {
	// Real example from KK7LLM-13. Coefficients are A, B, C per analog
	// channel (value = A*X² + B*X + C).
	d := Decode(":KK7LLM-13:EQNS.0,0.0293,0,0,0.879,-459", "APRS")
	if d.TelemConfig == nil {
		t.Fatalf("expected TelemConfig")
	}
	if d.TelemConfig.Kind != "eqns" {
		t.Errorf("Kind: got %q want %q", d.TelemConfig.Kind, "eqns")
	}
	// Channel 0: A=0, B=0.0293, C=0
	if d.TelemConfig.Coeffs[0][0] != 0 {
		t.Errorf("Coeffs[0][0]: got %v want 0", d.TelemConfig.Coeffs[0][0])
	}
	if !approx(d.TelemConfig.Coeffs[0][1], 0.0293, 1e-6) {
		t.Errorf("Coeffs[0][1]: got %v want 0.0293", d.TelemConfig.Coeffs[0][1])
	}
	if d.TelemConfig.Coeffs[0][2] != 0 {
		t.Errorf("Coeffs[0][2]: got %v want 0", d.TelemConfig.Coeffs[0][2])
	}
	// Channel 1: A=0, B=0.879, C=-459
	if !approx(d.TelemConfig.Coeffs[1][1], 0.879, 1e-6) {
		t.Errorf("Coeffs[1][1]: got %v want 0.879", d.TelemConfig.Coeffs[1][1])
	}
	if d.TelemConfig.Coeffs[1][2] != -459 {
		t.Errorf("Coeffs[1][2]: got %v want -459", d.TelemConfig.Coeffs[1][2])
	}
}

func TestTelemConfigBITS(t *testing.T) {
	d := Decode(":N0CALL   :BITS.10110010,My Project", "APRS")
	if d.TelemConfig == nil {
		t.Fatalf("expected TelemConfig")
	}
	if d.TelemConfig.Kind != "bits" {
		t.Errorf("Kind: got %q want %q", d.TelemConfig.Kind, "bits")
	}
	wantSense := [8]bool{true, false, true, true, false, false, true, false}
	if d.TelemConfig.Sense != wantSense {
		t.Errorf("Sense: got %v want %v", d.TelemConfig.Sense, wantSense)
	}
	if d.TelemConfig.Title != "My Project" {
		t.Errorf("Title: got %q want %q", d.TelemConfig.Title, "My Project")
	}
}

func TestTelemConfigBITSWithoutTitle(t *testing.T) {
	d := Decode(":N0CALL   :BITS.11111111", "APRS")
	if d.TelemConfig == nil {
		t.Fatalf("expected TelemConfig")
	}
	for i, b := range d.TelemConfig.Sense {
		if !b {
			t.Errorf("Sense[%d]: got false want true", i)
		}
	}
	if d.TelemConfig.Title != "" {
		t.Errorf("Title: got %q want empty", d.TelemConfig.Title)
	}
}

func TestTelemConfigBITSRejectsBadSense(t *testing.T) {
	// 8 chars required, all must be '0' or '1'. Non-binary or
	// wrong-length → reject (TelemConfig nil OR Kind unset).
	d := Decode(":N0CALL   :BITS.12345678,Title", "APRS")
	if d.TelemConfig != nil && d.TelemConfig.Kind == "bits" {
		t.Errorf("should reject BITS with non-binary chars")
	}
}

func TestTelemConfigNotConfusedWithRegularMessage(t *testing.T) {
	// A body that just happens to start with the letter P shouldn't match.
	d := Decode(":N0CALL   :Please call me back", "APRS")
	if d.TelemConfig != nil {
		t.Errorf("regular message should not produce TelemConfig")
	}
	if !d.IsMessage {
		t.Errorf("regular message should still be IsMessage=true")
	}
}

func TestTelemConfigIgnoresUnknownPrefix(t *testing.T) {
	// "TITLE." is sometimes seen but per spec belongs in BITS as a comma-
	// terminated trailing field. Standalone TITLE. is non-spec — treat as
	// a regular message.
	d := Decode(":N0CALL   :TITLE.My Station", "APRS")
	if d.TelemConfig != nil {
		t.Errorf("standalone TITLE. should not be parsed as telemetry config")
	}
}
