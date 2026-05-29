package webhook

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"

	"aprgo/internal/aprs"
	"aprgo/internal/ax25"
	"aprgo/internal/state"
)

// Match reports whether packet p should fire webhook wh. All configured
// filters AND together; an empty filter category means "don't care".
func Match(wh state.Webhook, p aprs.Packet) bool {
	origin := originStr(p.Frame.Origin)
	if !sourceMatches(wh, origin) {
		return false
	}
	if len(wh.Types) > 0 && !contains(wh.Types, classify(p)) {
		return false
	}
	if len(wh.Callsigns) > 0 && !callsignMatches(wh.Callsigns, effectiveSource(p)) {
		return false
	}
	// Destination: the message addressee. Non-message packets have an empty
	// MsgTo, which never matches a non-empty allowlist — so setting this
	// implicitly scopes the webhook to messages.
	if len(wh.ToCallsigns) > 0 && !callsignMatches(wh.ToCallsigns, p.Decoded.MsgTo) {
		return false
	}
	if !textMatches(wh, p) {
		return false
	}
	return true
}

// sourceMatches applies the RF/IS/Both selector plus the IncludeTX opt-in.
// TX-origin packets (our own transmissions) only fire when IncludeTX is set,
// regardless of the rf/is/both selector — they are neither RF- nor IS-received.
func sourceMatches(wh state.Webhook, origin string) bool {
	if origin == "tx" {
		return wh.IncludeTX
	}
	switch wh.Source {
	case "", "both":
		return true
	default:
		return wh.Source == origin
	}
}

// textMatches applies the message-body filter. A non-empty MatchText only
// ever matches message packets (a position beacon has no body to match).
func textMatches(wh state.Webhook, p aprs.Packet) bool {
	if wh.MatchText == "" {
		return true
	}
	if !p.Decoded.IsMessage {
		return false
	}
	body := p.Decoded.MsgBody
	want := wh.MatchText
	if !wh.MatchCase {
		body = strings.ToLower(body)
		want = strings.ToLower(want)
	}
	if wh.MatchMode == "equals" {
		return body == want
	}
	return strings.Contains(body, want) // "contains" is the default
}

// callsignMatches reports whether src matches any entry in the allowlist.
// A trailing "*" makes the entry a prefix wildcard. Case-insensitive.
func callsignMatches(list []string, src string) bool {
	up := strings.ToUpper(strings.TrimSpace(src))
	for _, c := range list {
		c = strings.ToUpper(strings.TrimSpace(c))
		if c == "" {
			continue
		}
		if strings.HasSuffix(c, "*") {
			if strings.HasPrefix(up, strings.TrimSuffix(c, "*")) {
				return true
			}
		} else if c == up {
			return true
		}
	}
	return false
}

// classify buckets a packet into one of the operator-facing type names used
// by the Types filter and the payload "type" field. Order matters: messages
// and weather/telemetry are checked before the generic position fallback.
func classify(p aprs.Packet) string {
	d := p.Decoded
	switch {
	case d.IsMessage && !d.IsAck && !d.IsRej:
		return "message"
	case d.Weather != nil:
		return "weather"
	case d.IsTelemetry:
		return "telemetry"
	case d.ObjectName != "":
		return "object"
	case d.Lat != nil && d.Lon != nil:
		return "position"
	case len(p.Frame.Info) > 0 && p.Frame.Info[0] == '>':
		return "status"
	default:
		return "other"
	}
}

// Types is the canonical list of classify() outputs, exported so the Settings
// page can render the type-filter checkboxes from one source of truth.
var Types = []string{"position", "weather", "telemetry", "message", "object", "status", "other"}

// effectiveSource returns the true originating station. For third-party /
// gated packets (info starts with `}`) the AX.25 source is the relay (the
// iGate that injected it) and the real originator is carried in MsgOrigSrc.
// Mirrors how server.parseLoop reports message sources, so the webhook
// filter and payload credit the sender, never the middle-man.
func effectiveSource(p aprs.Packet) string {
	if p.Decoded.MsgOrigSrc != "" {
		return p.Decoded.MsgOrigSrc
	}
	return p.Frame.Src
}

func originStr(o ax25.Source) string {
	switch o {
	case ax25.SrcRF:
		return "rf"
	case ax25.SrcIS:
		return "is"
	case ax25.SrcTX:
		return "tx"
	default:
		return ""
	}
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// payload is the JSON body POSTed to receivers. Flat and predictable so
// Home Assistant's webhook trigger can map fields directly. Optional fields
// are omitted when absent so receivers can rely on presence.
type payload struct {
	Time      string      `json:"time"` // RFC3339
	Source    string      `json:"source"`
	Dest      string      `json:"dest"`
	Path      []string    `json:"path,omitempty"`
	Origin    string      `json:"origin"`               // rf | is | tx
	RelayedBy string      `json:"relayed_by,omitempty"` // AX.25 relay for gated/3rd-party packets
	Type      string      `json:"type"`
	Lat       *float64    `json:"lat,omitempty"`
	Lon       *float64    `json:"lon,omitempty"`
	Symbol    string      `json:"symbol,omitempty"`
	Comment   string      `json:"comment,omitempty"`
	Speed     *int        `json:"speed_mph,omitempty"`
	Course    *int        `json:"course_deg,omitempty"`
	Altitude  *int        `json:"altitude_ft,omitempty"`
	Message   *msgPayload `json:"message,omitempty"`
	Raw       string      `json:"raw"`            // TNC2 form
	Test      bool        `json:"test,omitempty"` // true for "Send test" deliveries
}

type msgPayload struct {
	To   string `json:"to,omitempty"`
	Body string `json:"body,omitempty"`
	ID   string `json:"id,omitempty"`
}

// buildBody marshals a packet into the webhook JSON payload.
func buildBody(p aprs.Packet, test bool) ([]byte, error) {
	d := p.Decoded
	pl := payload{
		Time:    p.Frame.RxAt.UTC().Format(time.RFC3339),
		Source:  effectiveSource(p),
		Dest:    p.Frame.Dest,
		Path:    p.Frame.Path,
		Origin:  originStr(p.Frame.Origin),
		Type:    classify(p),
		Lat:     d.Lat,
		Lon:     d.Lon,
		Symbol:  d.Symbol,
		Comment: d.Comment,
		Raw:     p.Frame.TNC2(),
		Test:    test,
	}
	if d.Speed >= 0 {
		s := d.Speed
		pl.Speed = &s
	}
	if d.Course >= 0 {
		c := d.Course
		pl.Course = &c
	}
	if d.Altitude != 0 {
		a := d.Altitude
		pl.Altitude = &a
	}
	// When the packet was relayed/gated through a third party, Source is the
	// original sender (effectiveSource) and Frame.Src is the relay — expose
	// the relay separately so receivers keep full provenance.
	if d.MsgOrigSrc != "" && !strings.EqualFold(d.MsgOrigSrc, p.Frame.Src) {
		pl.RelayedBy = p.Frame.Src
	}
	if d.IsMessage && !d.IsAck && !d.IsRej {
		pl.Message = &msgPayload{To: d.MsgTo, Body: d.MsgBody, ID: d.MsgID}
	}
	// Use an encoder with HTML-escaping disabled so the TNC2 `raw` field reads
	// as literal `>`/`&`/`<` instead of `>` etc. The default json.Marshal
	// escapes those for safe HTML embedding, which we never do — this is an
	// application/json body. Encode appends a newline; trim it for a clean body.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(pl); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// sampleTestPacket builds a representative position packet for "Send test".
func sampleTestPacket() aprs.Packet {
	f := ax25.Frame{
		Src:    "N0CALL-9",
		Dest:   "APRGO",
		Path:   []string{"WIDE1-1"},
		Info:   []byte("!4903.50N/07201.75W>aprgo webhook test"),
		Origin: ax25.SrcRF,
		RxAt:   time.Now(),
	}
	return aprs.Parse(f)
}
