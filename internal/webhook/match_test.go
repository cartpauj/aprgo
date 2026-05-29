package webhook

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"aprgo/internal/aprs"
	"aprgo/internal/ax25"
	"aprgo/internal/state"
)

func pkt(origin ax25.Source, src, info string) aprs.Packet {
	return aprs.Parse(ax25.Frame{
		Src:    src,
		Dest:   "APRGO",
		Info:   []byte(info),
		Origin: origin,
		RxAt:   time.Unix(1700000000, 0),
	})
}

func TestSourceMatches(t *testing.T) {
	pos := "!4903.50N/07201.75W>hi"
	cases := []struct {
		name   string
		wh     state.Webhook
		origin ax25.Source
		want   bool
	}{
		{"both-rf", state.Webhook{Source: "both"}, ax25.SrcRF, true},
		{"both-is", state.Webhook{Source: "both"}, ax25.SrcIS, true},
		{"empty-defaults-both", state.Webhook{Source: ""}, ax25.SrcIS, true},
		{"rf-only-rf", state.Webhook{Source: "rf"}, ax25.SrcRF, true},
		{"rf-only-is", state.Webhook{Source: "rf"}, ax25.SrcIS, false},
		{"is-only-rf", state.Webhook{Source: "is"}, ax25.SrcRF, false},
		{"tx-excluded-by-default", state.Webhook{Source: "both"}, ax25.SrcTX, false},
		{"tx-included-when-opted-in", state.Webhook{Source: "both", IncludeTX: true}, ax25.SrcTX, true},
		{"tx-include-ignores-source-rf", state.Webhook{Source: "rf", IncludeTX: true}, ax25.SrcTX, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Match(c.wh, pkt(c.origin, "N0CALL", pos)); got != c.want {
				t.Fatalf("Match=%v want %v", got, c.want)
			}
		})
	}
}

func TestTypeFilter(t *testing.T) {
	position := pkt(ax25.SrcRF, "N0CALL", "!4903.50N/07201.75W>hi")
	message := pkt(ax25.SrcRF, "N0CALL", ":KC9XYZ   :hello{1")

	if classify(position) != "position" {
		t.Fatalf("classify position = %q", classify(position))
	}
	if classify(message) != "message" {
		t.Fatalf("classify message = %q", classify(message))
	}

	// Filter restricted to "message" rejects a position, accepts a message.
	wh := state.Webhook{Source: "both", Types: []string{"message"}}
	if Match(wh, position) {
		t.Fatal("position should not match a message-only filter")
	}
	if !Match(wh, message) {
		t.Fatal("message should match a message-only filter")
	}

	// Empty Types = all.
	if !Match(state.Webhook{Source: "both"}, position) {
		t.Fatal("empty type filter should match everything")
	}
}

func TestCallsignFilter(t *testing.T) {
	p := pkt(ax25.SrcRF, "N0CALL-9", "!4903.50N/07201.75W>hi")
	cases := []struct {
		list []string
		want bool
	}{
		{nil, true},
		{[]string{"N0CALL-9"}, true},
		{[]string{"n0call-9"}, true}, // case-insensitive
		{[]string{"W1AW", "N0CALL*"}, true},
		{[]string{"N0CALL-1"}, false},
		{[]string{"W1AW"}, false},
		{[]string{"N0*"}, true},
	}
	for _, c := range cases {
		wh := state.Webhook{Source: "both", Callsigns: c.list}
		if got := Match(wh, p); got != c.want {
			t.Fatalf("callsigns=%v Match=%v want %v", c.list, got, c.want)
		}
	}
}

func TestToCallsignFilter(t *testing.T) {
	toMe := pkt(ax25.SrcRF, "KC9XYZ", ":N0CALL-7 :ping{4")    // addressed to N0CALL-7
	toOther := pkt(ax25.SrcRF, "KC9XYZ", ":W1AW     :ping{5") // addressed to W1AW
	position := pkt(ax25.SrcRF, "N0CALL-7", "!4903.50N/07201.75W>hi")

	wh := state.Webhook{Source: "both", ToCallsigns: []string{"N0CALL-7"}}
	if !Match(wh, toMe) {
		t.Fatal("message addressed to N0CALL-7 should match To filter")
	}
	if Match(wh, toOther) {
		t.Fatal("message addressed to W1AW should not match To=N0CALL-7")
	}
	// A non-message packet has no addressee, so a non-empty To filter must
	// exclude it (even though its SOURCE is N0CALL-7).
	if Match(wh, position) {
		t.Fatal("position packet has no addressee; To filter must exclude it")
	}
	// Wildcard.
	if !Match(state.Webhook{Source: "both", ToCallsigns: []string{"N0CALL*"}}, toMe) {
		t.Fatal("N0CALL* should match addressee N0CALL-7")
	}
	// From and To are independent axes.
	if !Match(state.Webhook{Source: "both", Callsigns: []string{"KC9XYZ"}, ToCallsigns: []string{"N0CALL-7"}}, toMe) {
		t.Fatal("From=KC9XYZ + To=N0CALL-7 should match a KC9XYZ→N0CALL-7 message")
	}
}

// TestThirdPartySourceIsOriginator verifies that for a gated/third-party
// packet the From filter and payload credit the real originator, not the
// relay (middle-man) in the AX.25 source.
func TestThirdPartySourceIsOriginator(t *testing.T) {
	// RELAY-1 (the AX.25 source) gated a message that WB2OSZ-5 originally
	// sent to KG7OKR-10.
	p := pkt(ax25.SrcIS, "RELAY-1", "}WB2OSZ-5>APRS,TCPIP*::KG7OKR-10:hello{1")
	if p.Decoded.MsgOrigSrc != "WB2OSZ-5" {
		t.Fatalf("decode sanity: MsgOrigSrc = %q, want WB2OSZ-5", p.Decoded.MsgOrigSrc)
	}
	if effectiveSource(p) != "WB2OSZ-5" {
		t.Fatalf("effectiveSource = %q, want WB2OSZ-5 (the originator)", effectiveSource(p))
	}

	// From filter matches the originator, NOT the relay.
	if !Match(state.Webhook{Source: "both", Callsigns: []string{"WB2OSZ-5"}}, p) {
		t.Error("From=WB2OSZ-5 should match the original sender")
	}
	if Match(state.Webhook{Source: "both", Callsigns: []string{"RELAY-1"}}, p) {
		t.Error("From=RELAY-1 (the relay) must NOT match — that's the middle-man")
	}

	// Payload credits the originator and records the relay separately.
	b, err := buildBody(p, false)
	if err != nil {
		t.Fatal(err)
	}
	var got payload
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Source != "WB2OSZ-5" {
		t.Errorf("payload source = %q, want WB2OSZ-5", got.Source)
	}
	if got.RelayedBy != "RELAY-1" {
		t.Errorf("payload relayed_by = %q, want RELAY-1", got.RelayedBy)
	}
	if got.Message == nil || got.Message.To != "KG7OKR-10" {
		t.Errorf("message.to = %+v, want KG7OKR-10", got.Message)
	}
}

func TestMessageTextFilter(t *testing.T) {
	openMsg := pkt(ax25.SrcRF, "N0CALL", ":KC9XYZ   :OPEN GARAGE{7")
	otherMsg := pkt(ax25.SrcRF, "N0CALL", ":KC9XYZ   :hello there{8")
	position := pkt(ax25.SrcRF, "N0CALL", "!4903.50N/07201.75W>hi")

	contains := state.Webhook{Source: "both", MatchMode: "contains", MatchText: "garage"}
	if !Match(contains, openMsg) {
		t.Fatal("contains 'garage' should match OPEN GARAGE (case-insensitive)")
	}
	if Match(contains, otherMsg) {
		t.Fatal("contains 'garage' should not match unrelated message")
	}
	// A non-message packet never matches a text filter.
	if Match(contains, position) {
		t.Fatal("text filter must not match a position packet")
	}

	equals := state.Webhook{Source: "both", MatchMode: "equals", MatchText: "OPEN GARAGE"}
	if !Match(equals, openMsg) {
		t.Fatal("equals should match exact body")
	}
	if Match(state.Webhook{Source: "both", MatchMode: "equals", MatchText: "OPEN"}, openMsg) {
		t.Fatal("equals should require the whole body")
	}

	// Case-sensitive contains.
	cs := state.Webhook{Source: "both", MatchMode: "contains", MatchText: "garage", MatchCase: true}
	if Match(cs, openMsg) {
		t.Fatal("case-sensitive 'garage' should not match 'GARAGE'")
	}
}

func TestBuildBodyPosition(t *testing.T) {
	p := pkt(ax25.SrcRF, "N0CALL-9", "!4903.50N/07201.75W>moving")
	b, err := buildBody(p, false)
	if err != nil {
		t.Fatal(err)
	}
	var got payload
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Source != "N0CALL-9" {
		t.Errorf("source = %q", got.Source)
	}
	if got.Origin != "rf" {
		t.Errorf("origin = %q", got.Origin)
	}
	if got.Type != "position" {
		t.Errorf("type = %q", got.Type)
	}
	if got.Lat == nil || got.Lon == nil {
		t.Fatalf("expected lat/lon, got %+v / %+v", got.Lat, got.Lon)
	}
	if got.Raw == "" {
		t.Error("raw TNC2 should be populated")
	}
	if got.Test {
		t.Error("test flag should be false")
	}
	// The TNC2 raw field's `src>dest` separator must not be HTML-escaped:
	// the body is application/json, never embedded in HTML.
	if bytes.Contains(b, []byte("\\u003e")) {
		t.Errorf("raw must not HTML-escape '>' as \\u003e: %s", b)
	}
	if !bytes.Contains(b, []byte("N0CALL-9>APRGO")) {
		t.Errorf("raw TNC2 should contain literal 'N0CALL-9>APRGO': %s", b)
	}
}

func TestBuildBodyMessage(t *testing.T) {
	p := pkt(ax25.SrcIS, "N0CALL", ":KC9XYZ   :hello{1")
	b, err := buildBody(p, true)
	if err != nil {
		t.Fatal(err)
	}
	var got payload
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Origin != "is" {
		t.Errorf("origin = %q", got.Origin)
	}
	if got.Type != "message" {
		t.Errorf("type = %q", got.Type)
	}
	if got.Message == nil {
		t.Fatal("expected message payload")
	}
	if got.Message.Body != "hello" {
		t.Errorf("message body = %q", got.Message.Body)
	}
	if !got.Test {
		t.Error("test flag should be true")
	}
}
