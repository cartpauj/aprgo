package aprs

import "testing"

// Object and item names — APRS101 §11 (objects) and §11.2 (items).
//
//   Object: `;NNNNNNNNN*HHMMSS<pos>` — name is exactly 9 chars, space-padded.
//   Item:   `)NAME!<pos>` or `)NAME_<pos>` — name is 3-9 chars, terminated by
//           the `!` (alive) or `_` (killed) live/kill indicator.
//
// These tests cover real-world traffic patterns observed on RF (446.25HRM
// repeater, NOAA weather stations, club beacons named "WX" / "SLC", etc.)
// — the operator-visible value of an object/item is its name, so silently
// dropping it makes the dashboard much less useful.

func TestObjectNameExtracted(t *testing.T) {
	d := Decode(";446.25HRM*111111z4031.03N/11200.83WrT11", "APRS")
	if d.ObjectName != "446.25HRM" {
		t.Errorf("ObjectName: got %q want %q", d.ObjectName, "446.25HRM")
	}
	if d.ObjectKilled {
		t.Errorf("expected live object (*)")
	}
}

func TestObjectNameTrimmedOfTrailingSpace(t *testing.T) {
	// Object names are space-padded to 9 chars. Trim trailing spaces.
	d := Decode(";WX       *111111z4031.03N/11200.83W_", "APRS")
	if d.ObjectName != "WX" {
		t.Errorf("ObjectName: got %q want %q", d.ObjectName, "WX")
	}
}

func TestObjectKilledFlagWithName(t *testing.T) {
	d := Decode(";446.25HRM_111111z4031.03N/11200.83WrT11", "APRS")
	if d.ObjectName != "446.25HRM" {
		t.Errorf("name should still extract on killed object: got %q", d.ObjectName)
	}
	if !d.ObjectKilled {
		t.Errorf("expected ObjectKilled=true for '_'")
	}
}

func TestItemNameExtractedAlive(t *testing.T) {
	// Item format: `)NAME!<position>`. Name is 3-9 chars.
	d := Decode(")TestItem!4031.03N/11200.83Wr", "APRS")
	if d.ObjectName != "TestItem" {
		t.Errorf("ItemName: got %q want %q", d.ObjectName, "TestItem")
	}
	if d.ObjectKilled {
		t.Errorf("`!` indicates alive — should not be killed")
	}
}

func TestItemNameExtractedKilled(t *testing.T) {
	d := Decode(")TestItem_4031.03N/11200.83Wr", "APRS")
	if d.ObjectName != "TestItem" {
		t.Errorf("ItemName: got %q want %q", d.ObjectName, "TestItem")
	}
	if !d.ObjectKilled {
		t.Errorf("`_` indicates killed — should set ObjectKilled")
	}
}

func TestItemNameShortest(t *testing.T) {
	// Item names are 3-9 chars per spec; 3 chars is the minimum.
	d := Decode(")ABC!4031.03N/11200.83Wr", "APRS")
	if d.ObjectName != "ABC" {
		t.Errorf("short item name: got %q want %q", d.ObjectName, "ABC")
	}
}

func TestObjectTooShortNoName(t *testing.T) {
	// If the packet is malformed (too short to contain a full object header),
	// nothing should be extracted — fail-soft.
	d := Decode(";short", "APRS")
	if d.ObjectName != "" {
		t.Errorf("expected no name on truncated object, got %q", d.ObjectName)
	}
}
