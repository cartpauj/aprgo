package server

import (
	"context"
	"log"
	"math/rand"
	"strings"
	"time"

	"aprgo/internal/aprs"
	"aprgo/internal/ax25"
	"aprgo/internal/gate"
	"aprgo/internal/state"
)

// maybeAnswerQuery inspects an RF-received packet for a general APRS query
// ("?IGATE?" or "?APRS?") and, if found, schedules a response after a random
// 0-30s delay (collision-avoidance — without this, every iGate in earshot
// would key up simultaneously and stomp each other). Only general queries
// are handled here; directed queries (the "ADDR :?APRSD" message form) are
// not implemented in this revision.
//
// Per APRS101 Ch. 15: an iGate must answer queries received via RF; queries
// received via IS are ignored. Responses use the spec-defined formats:
//
//	?IGATE? → "<IGATE,MSG_CNT=n,LOC_CNT=n" (DTI `<`, capabilities frame)
//	?APRS?  → triggers a position beacon
func (s *Server) maybeAnswerQuery(ctx context.Context, pkt aprs.Packet, snap state.State) {
	if pkt.Frame.Origin != ax25.SrcRF {
		return
	}
	if !snap.TXEnable || snap.Callsign == "" {
		return
	}
	// Ignore our own query echo.
	if strings.EqualFold(pkt.Frame.Src, snap.Callsign) {
		return
	}
	info := strings.TrimRight(string(pkt.Frame.Info), "\r\n ")
	if info == "" || info[0] != '?' {
		return
	}
	switch strings.ToUpper(info) {
	case "?IGATE?":
		s.scheduleQueryResponse(ctx, func() { s.respondIGateCapabilities(snap) })
	case "?APRS?":
		s.scheduleQueryResponse(ctx, func() { _ = s.beacon.TransmitNow("position") })
	}
}

// scheduleQueryResponse delays the response by 0-30s to avoid pile-ups
// when multiple iGates hear the same query (APRS convention; spec is silent
// on the exact figure, the random window is community practice).
//
// Cancellation: the callback checks ctx.Err() before doing the side effect.
// This is the canonical Go idiom — Timer.Stop() races against the timer's
// own goroutine (returns false if the callback has already started but
// hasn't executed), so the ctx check is the authoritative gate that prevents
// post-shutdown transmissions. We skip the Stop()+watcher-goroutine
// optimization: query traffic is low-volume so timer GC isn't worth a
// permanent goroutine per scheduled response.
func (s *Server) scheduleQueryResponse(ctx context.Context, fn func()) {
	delay := time.Duration(rand.Intn(30000)) * time.Millisecond
	time.AfterFunc(delay, func() {
		if ctx.Err() != nil {
			return
		}
		fn()
	})
}

// respondIGateCapabilities emits a `<IGATE,MSG_CNT=n,LOC_CNT=n` frame per
// APRS101 §15. MSG_CNT is messages we've gated to RF (excluding digipeats);
// LOC_CNT is the count of distinct stations heard on RF within the
// configured "recent" window (same window used for IS→RF gating decisions).
func (s *Server) respondIGateCapabilities(snap state.State) {
	// MSG_CNT: count of messages gated IS→RF, read from the dedicated
	// counter. Computing this as sentRF - digipeats would underflow if a
	// digipeat increment landed between the two atomic loads (atomics give
	// per-variable linearizability, not snapshot consistency).
	msgCnt := s.stats.igateMsgsRF.Load()
	locCnt := s.store.CountHeardOnRF(time.Now().Add(-recentRFWindow(snap)))
	info := []byte("<IGATE,MSG_CNT=" + uintToString(msgCnt) + ",LOC_CNT=" + intToString(locCnt))
	a := gate.Action{
		Kind:   gate.SendRF,
		RFSrc:  snap.Callsign,
		RFDest: state.DefaultBeaconDest,
		RFPath: nil,
		RFInfo: info,
		Reason: "?IGATE? reply",
	}
	if err := s.dispatchSendRF(a); err != nil {
		log.Printf("query: ?IGATE? reply failed: %v", err)
	}
}

func uintToString(u uint64) string {
	if u == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for u > 0 {
		i--
		buf[i] = byte('0' + u%10)
		u /= 10
	}
	return string(buf[i:])
}

func intToString(n int) string {
	if n < 0 {
		return "0"
	}
	return uintToString(uint64(n))
}
