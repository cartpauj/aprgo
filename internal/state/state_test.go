package state

import "testing"

// 49°03.50'N, 072°01.75'W with symbol "I&" (iGate diamond), messaging on.
// Full-precision baseline: "=4903.50NI07201.75W&"
func TestComposeInfoFullPrecision(t *testing.T) {
	b := Beacon{Symbol: "I&", Messages: true}
	got := b.ComposeInfo(49.0583333, -72.0291666)
	want := "=4903.50NI07201.75W&"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestComposeInfoAmbiguityLevels(t *testing.T) {
	cases := []struct {
		level int
		want  string
	}{
		{0, "=4903.50NI07201.75W&"},
		{1, "=4903.5 NI07201.7 W&"},
		{2, "=4903.  NI07201.  W&"},
		{3, "=490 .  NI0720 .  W&"},
		{4, "=49  .  NI072  .  W&"},
	}
	for _, c := range cases {
		b := Beacon{Symbol: "I&", Messages: true, AmbiguityLevel: c.level}
		got := b.ComposeInfo(49.0583333, -72.0291666)
		if got != c.want {
			t.Errorf("level=%d: got %q, want %q", c.level, got, c.want)
		}
	}
}

// Out-of-range level should clamp to 4, not panic or corrupt the output.
func TestComposeInfoAmbiguityClamped(t *testing.T) {
	b := Beacon{Symbol: "I&", Messages: true, AmbiguityLevel: 99}
	got := b.ComposeInfo(49.0583333, -72.0291666)
	want := "=49  .  NI072  .  W&"
	if got != want {
		t.Fatalf("clamped: got %q, want %q", got, want)
	}
}
