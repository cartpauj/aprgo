// Package gate is aprgo's RF↔IS gating + digipeater decision engine.
//
// Decide() is mostly pure: it takes a parsed packet, a state snapshot, and a
// HeardOnRF lookup callback. It returns the set of actions to dispatch.
// All policy lives here; the caller just executes the returned Actions.
package gate

import (
	"fmt"
	"strconv"
	"strings"

	"aprgo/internal/aprs"
	"aprgo/internal/ax25"
	"aprgo/internal/state"
)

// AX.25 path length cap. We refuse to construct a digipeat output longer
// than this — partly spec compliance, partly anti-spam.
const maxPathHops = 8

// Max N value we'll accept on a WIDEn-N path. WIDE2-3+ requests are
// considered abusive (the operator is asking for more hops than the
// network can support); standard practice is to silently drop them.
const maxWIDEHops = 2

// HeardChecker returns true if `call` has been heard on RF recently.
// Pass a noop (always-false) checker if the store isn't ready yet.
type HeardChecker func(call string) bool

// ActionKind is the category of action to dispatch.
type ActionKind uint8

const (
	Drop ActionKind = iota
	SendIS
	// SendRF: dispatcher constructs AX.25 from RFSrc/RFDest/RFPath/RFInfo and calls rf.TX.
	SendRF
)

// Action describes one thing for the dispatcher to do.
type Action struct {
	Kind ActionKind

	// SendIS: serialized TNC2 line.
	Payload string

	// SendRF: AX.25 fields. Either RFRaw is set (re-TX existing bytes, for digipeating)
	// or RFSrc/RFDest/RFPath/RFInfo are set (build new frame, for IS→RF).
	RFRaw  []byte
	RFSrc  string
	RFDest string
	RFPath []string
	RFInfo []byte

	// Viscous: SendRF only. When true, the dispatcher should hold the TX
	// for a randomized 3–5 s window and cancel if it hears the same content
	// from another station during the hold. Used for fill-in WIDE1-1
	// digipeating — lets higher-elevation digis pre-empt our copy and keeps
	// the channel from getting double-stomped.
	Viscous bool

	Reason string // human-readable, for UI/metrics
}

// Decide returns the actions to perform for the given packet.
func Decide(p aprs.Packet, s state.State, heardOnRF HeardChecker) []Action {
	if heardOnRF == nil {
		heardOnRF = func(string) bool { return false }
	}
	switch p.Frame.Origin {
	case ax25.SrcRF:
		return decideFromRF(p, s)
	case ax25.SrcIS:
		return decideFromIS(p, s, heardOnRF)
	case ax25.SrcTX:
		// Synthesized echo of our own transmission, published purely so
		// the dashboard live-feed can show it. No gating decision applies.
		return nil
	}
	return []Action{{Kind: Drop, Reason: "unknown origin"}}
}

func decideFromRF(p aprs.Packet, s state.State) []Action {
	var actions []Action

	// Digipeat decision (independent of gating)
	if (s.DigipeatWIDE1 || s.DigipeatWIDE2 || s.PreemptiveDigipeat) && s.TXEnable && p.Frame.Raw != nil {
		if a := digipeatAction(p, s); a != nil {
			actions = append(actions, *a)
		}
	}

	// Gate-to-IS decision
	if s.GateRFtoIS {
		if a := rfToISAction(p, s); a != nil {
			actions = append(actions, *a)
		} else {
			actions = append(actions, Action{Kind: Drop, Reason: "RF→IS filtered"})
		}
	}
	if len(actions) == 0 {
		actions = append(actions, Action{Kind: Drop, Reason: "no actions configured for RF"})
	}
	return actions
}

func rfToISAction(p aprs.Packet, s state.State) *Action {
	if strings.EqualFold(p.Frame.Src, s.Callsign) {
		return nil
	}
	// Messaging-only mode: skip every RF→IS gate except actual message +
	// ack frames. The station still participates in person-to-person APRS
	// chat as a bridge, but doesn't add position-beacon / weather /
	// telemetry / status noise to the IS firehose.
	if s.MessagingOnlyMode && !p.Decoded.IsMessage && !p.Decoded.IsAck {
		return nil
	}
	for _, hop := range p.Frame.Path {
		hopUp := strings.ToUpper(strings.TrimSuffix(hop, "*"))
		// IS-side path tokens (handles both TCPIP and TCPIP* via TrimSuffix above)
		if hopUp == "NOGATE" || hopUp == "RFONLY" || hopUp == "TCPIP" || hopUp == "TCPXX" {
			return nil
		}
		// q-constructs (qAR, qAS, qAC, qAO, qAU, qAX) — packet already gated by someone else
		if len(hopUp) == 3 && hopUp[0] == 'Q' && hopUp[1] == 'A' {
			return nil
		}
	}
	if len(p.Frame.Info) == 0 {
		return nil
	}
	// Skip queries and packets already wrapped as third-party (loop prevention)
	if p.Frame.Info[0] == '?' || p.Frame.Info[0] == '}' {
		return nil
	}
	tnc2 := buildGatedTNC2(p, s)
	return &Action{Kind: SendIS, Payload: tnc2, Reason: "RF→IS"}
}

// digipeatAction decides whether to digipeat `p` and returns the resulting
// SendRF Action (with re-encoded AX.25), or nil to skip.
//
// Implements the APRS "New-N Paradigm" decrement for WIDEn-N tokens:
//
//	WIDEn-N   →  if N > 1: MYCALL*, WIDEn-(N-1)
//	          →  if N = 1: MYCALL*               (last hop)
//
// Eligibility is split: DigipeatWIDE1 allows handling WIDE1-N (fill-in
// role); DigipeatWIDE2 allows handling WIDE2-N (full-digi role).
//
// Refusals (all silent — no Drop emitted):
//   - Our callsign already appears anywhere in the path (we handled it)
//   - First unused hop is not a WIDEn-N we're configured for
//   - N > maxWIDEHops (currently 2 — anti-spam trap)
//   - Resulting path would exceed maxPathHops
//   - Source equals our callsign (don't re-digi own beacons)
func digipeatAction(p aprs.Packet, s state.State) *Action {
	if s.Callsign == "" || strings.EqualFold(p.Frame.Src, s.Callsign) {
		return nil
	}
	myCallUp := strings.ToUpper(s.Callsign)

	// Scan path once: find where MYCALL appears (if at all) and the
	// first unused hop.
	myCallIdx := -1
	myCallAlreadyUsed := false
	idx := -1
	for i, hop := range p.Frame.Path {
		used := strings.HasSuffix(hop, "*")
		if idx < 0 && !used {
			idx = i
		}
		if myCallIdx < 0 && strings.ToUpper(strings.TrimSuffix(hop, "*")) == myCallUp {
			myCallIdx = i
			myCallAlreadyUsed = used
		}
	}

	// MYCALL is in the path with the used-bit set: we already digipeated
	// this one (or someone is forging us). Either way, don't re-act.
	if myCallIdx >= 0 && myCallAlreadyUsed {
		return nil
	}

	// MYCALL is in the unused portion of the path — preemptive case.
	// Per spec, this is only honored when the operator opts in; never
	// applied to generic WIDEn-N tokens (parseWIDE skips those above).
	if myCallIdx >= 0 {
		if !s.PreemptiveDigipeat {
			return nil
		}
		newPath := buildPreemptPath(p.Frame.Path, myCallIdx, s.Callsign)
		if len(newPath) > maxPathHops {
			return nil
		}
		raw, err := ax25.EncodeUIFrame(p.Frame.Src, p.Frame.Dest, newPath, p.Frame.Info)
		if err != nil {
			return nil
		}
		return &Action{
			Kind:   SendRF,
			RFRaw:  raw,
			Reason: "preempt digi",
		}
	}

	if idx < 0 {
		return nil
	}
	n, N, ok := parseWIDE(p.Frame.Path[idx])
	if !ok {
		return nil
	}
	// Eligibility per role.
	switch {
	case n == 1 && N == 1 && !s.DigipeatWIDE1:
		return nil
	case n == 1 && N != 1:
		return nil // WIDE1-N with N≠1 is non-standard
	case n == 2 && (!s.DigipeatWIDE2 || N < 1 || N > maxWIDEHops):
		return nil
	case n >= 3:
		return nil // refuse legacy abusive widths
	}

	myCallUsed := s.Callsign + "*"
	var newPath []string
	if N <= 1 {
		// Last hop — replace WIDEn-N entirely with our used-marker.
		newPath = make([]string, len(p.Frame.Path))
		copy(newPath, p.Frame.Path)
		newPath[idx] = myCallUsed
	} else {
		// N > 1 — decrement and prepend our used-marker. The remaining
		// WIDEn-(N-1) token tells downstream digis there are hops left.
		newPath = make([]string, 0, len(p.Frame.Path)+1)
		newPath = append(newPath, p.Frame.Path[:idx]...)
		newPath = append(newPath, myCallUsed)
		newPath = append(newPath, fmt.Sprintf("WIDE%d-%d", n, N-1))
		newPath = append(newPath, p.Frame.Path[idx+1:]...)
	}
	if len(newPath) > maxPathHops {
		return nil
	}

	raw, err := ax25.EncodeUIFrame(p.Frame.Src, p.Frame.Dest, newPath, p.Frame.Info)
	if err != nil {
		return nil
	}
	// Mark fill-in WIDE1-1 digipeats as viscous when configured. WIDE2-N
	// is authoritative (only full-digis handle it; nobody to defer to),
	// so it's never viscous-delayed.
	viscous := s.ViscousDelay && n == 1
	return &Action{
		Kind:    SendRF,
		RFRaw:   raw,
		Viscous: viscous,
		Reason:  fmt.Sprintf("digi WIDE%d-%d", n, N),
	}
}

// buildPreemptPath constructs the new path for a preemptive digipeat in
// MARK mode (APRS 1.2 §preemptive-digipeating): every prior unused hop
// gets the has-been-digipeated bit set, MYCALL takes the slot it occupied
// in the path with that bit set, and hops after MYCALL are left alone for
// downstream digis to consume.
func buildPreemptPath(path []string, myCallIdx int, myCall string) []string {
	out := make([]string, len(path))
	for i, hop := range path {
		switch {
		case i < myCallIdx:
			if strings.HasSuffix(hop, "*") {
				out[i] = hop
			} else {
				out[i] = hop + "*"
			}
		case i == myCallIdx:
			out[i] = myCall + "*"
		default:
			out[i] = hop
		}
	}
	return out
}

// parseWIDE extracts (n, N) from a hop token of the form "WIDEn-N".
// Returns ok=false for anything else, including used-bit variants like
// "WIDE2-1*" (we only inspect unused hops at the call site).
func parseWIDE(hop string) (n, N int, ok bool) {
	if !strings.HasPrefix(strings.ToUpper(hop), "WIDE") {
		return 0, 0, false
	}
	rest := hop[4:]
	dash := strings.IndexByte(rest, '-')
	if dash <= 0 || dash == len(rest)-1 {
		return 0, 0, false
	}
	var err error
	if n, err = strconv.Atoi(rest[:dash]); err != nil || n < 1 || n > 7 {
		return 0, 0, false
	}
	if N, err = strconv.Atoi(rest[dash+1:]); err != nil || N < 0 || N > 7 {
		return 0, 0, false
	}
	return n, N, true
}

func decideFromIS(p aprs.Packet, s state.State, heardOnRF HeardChecker) []Action {
	if !s.GateIStoRF || !s.TXEnable {
		return []Action{{Kind: Drop, Reason: "IS→RF disabled"}}
	}
	if s.Callsign == "" {
		return []Action{{Kind: Drop, Reason: "no local callsign"}}
	}
	// Never gate our own packets that round-tripped through IS back to RF.
	if strings.EqualFold(p.Frame.Src, s.Callsign) {
		return []Action{{Kind: Drop, Reason: "IS→RF: own packet from IS"}}
	}

	// IS→RF policy: only relay APRS messages addressed to a callsign
	// recently heard on RF. (Avoids dumping arbitrary IS traffic on the air.)
	if !p.Decoded.IsMessage {
		return []Action{{Kind: Drop, Reason: "IS→RF: not a message"}}
	}
	if p.Decoded.MsgTo == "" {
		return []Action{{Kind: Drop, Reason: "IS→RF: no recipient"}}
	}
	if strings.EqualFold(p.Decoded.MsgTo, s.Callsign) {
		return []Action{{Kind: Drop, Reason: "IS→RF: addressed to us"}}
	}
	// Self-addressed messages (source == recipient) are config/telemetry
	// declarations, never person-to-person traffic that deserves RF relay.
	if strings.EqualFold(p.Decoded.MsgTo, p.Frame.Src) {
		return []Action{{Kind: Drop, Reason: "IS→RF: self-addressed (telemetry/config)"}}
	}
	// Skip telemetry parameter/unit/equations/bits declarations even if not
	// self-addressed — they're meta-data, not communication.
	bodyUp := strings.ToUpper(p.Decoded.MsgBody)
	for _, pfx := range []string{"PARM.", "UNIT.", "EQNS.", "BITS."} {
		if strings.HasPrefix(bodyUp, pfx) {
			return []Action{{Kind: Drop, Reason: "IS→RF: telemetry " + pfx[:4] + " declaration"}}
		}
	}
	// Bulletins (recipient starts with "BLN") are broadcasts, not directed
	// messages. Standard iGate practice is not to relay them to RF.
	if strings.HasPrefix(strings.ToUpper(p.Decoded.MsgTo), "BLN") {
		return []Action{{Kind: Drop, Reason: "IS→RF: bulletin"}}
	}
	if !heardOnRF(p.Decoded.MsgTo) {
		return []Action{{Kind: Drop, Reason: "IS→RF: recipient not heard on RF"}}
	}

	// Third-party form per convention: }SRC>DEST,TCPIP*,MYCALL*:info
	// The used-bit (*) on TCPIP marks the IS leg as already traversed; MYCALL*
	// marks us as the gateway that injected the frame.
	dest := state.DefaultBeaconDest
	thirdParty := "}" + p.Frame.Src + ">" + p.Frame.Dest + ",TCPIP*," + s.Callsign + "*:" + string(p.Frame.Info)
	return []Action{{
		Kind:   SendRF,
		RFSrc:  s.Callsign,
		RFDest: dest,
		RFPath: nil,
		RFInfo: []byte(thirdParty),
		Reason: "IS→RF msg→" + p.Decoded.MsgTo,
	}}
}

// buildGatedTNC2 produces the APRS-IS form of an RF-originated packet,
// inserting a q-construct ("qAR" = gated by an RF-equipped iGate) and our
// callsign before the info field.
func buildGatedTNC2(p aprs.Packet, s state.State) string {
	var b strings.Builder
	b.WriteString(p.Frame.Src)
	b.WriteByte('>')
	b.WriteString(p.Frame.Dest)
	for _, hop := range p.Frame.Path {
		b.WriteByte(',')
		b.WriteString(hop)
	}
	b.WriteByte(',')
	b.WriteString("qAR,")
	b.WriteString(s.Callsign)
	b.WriteByte(':')
	b.Write(p.Frame.Info)
	return b.String()
}
