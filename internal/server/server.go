// Package server is aprgo's top-level orchestrator: HTTP, the parse pipeline,
// gating dispatcher, RF + IS subprocesses, state + store.
package server

import (
	"context"
	"crypto/sha1"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"math"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"aprgo/internal/aprs"
	"aprgo/internal/auth"
	"aprgo/internal/ax25"
	"aprgo/internal/beacon"
	"aprgo/internal/bus"
	"aprgo/internal/gate"
	"aprgo/internal/igate"
	"aprgo/internal/rf"
	"aprgo/internal/state"
	"aprgo/internal/store"
	"aprgo/web"
)

// Options configures the server.
type Options struct {
	Listen    string
	StatePath string
	DBPath    string
}

// Server is the long-lived application instance.
type Server struct {
	opts   Options
	state  *state.Store
	auth   *auth.Authenticator
	bus    *bus.Bus
	store  *store.Store
	rf     *rf.RF
	is     *igate.Client
	beacon *beacon.Beacon
	tmpl   *template.Template
	static map[string]staticAsset

	dupe *dupeTable
	// msgDupe collapses "same message body, different path" repeats so a
	// single SMS reply heard via 6 iGate paths only logs once.
	msgDupe *dupeTable
	// msgBodyDupe is the fallback dedupe keyed only on (source,dest,body)
	// — catches retries from radios that increment msg_id, which slips
	// past msgDupe. 15-minute window because some radios keep retrying
	// for several minutes when an ack never arrives.
	msgBodyDupe *dupeTable

	mu     sync.Mutex
	recent []aprs.Packet
	// recentNextID is the monotonic counter assigned to the *next* packet
	// appended to `recent`. The packet at index i in `recent` has ID
	// (recentNextID - uint64(len(recent)) + uint64(i) + 1). Polling clients
	// pass back the last ID they saw and the server returns everything newer.
	recentNextID uint64

	// wizard draft state, keyed by session cookie value.
	wmu     sync.Mutex
	wdrafts map[string]*wizardDraft

	// Serializes wizard BT scans so concurrent UI clicks don't trash BlueZ.
	scanMu sync.Mutex

	// retries holds outbound messages awaiting ack. See retry.go.
	retries *retryQueue

	// viscous holds fill-in WIDE1-1 digipeats during their 3–5 s polite
	// window so they can be cancelled if a higher-elevation digi handles
	// the same content first. See viscous.go.
	viscous *viscousQueue

	// drops holds the last N gate-drop reasons for the /diagnostics page.
	drops *dropRing

	// msgTracker remembers callsigns we recently IS→RF gated a message FROM
	// so the next position packet for each can be gated to RF once (per
	// APRS-IS IGating.aspx "responder position" courtesy).
	msgTracker *gate.MessagedRecipientTracker

	// bulletinSends enforces a per-identifier cooldown on outbound
	// bulletin TX (5 min) so accidental double-clicks on the compose
	// form don't flood the channel.
	bulletinSends *bulletinSendTracker

	loginLimit *loginLimiter

	// In-flight async Bluetooth pair operations, keyed by session cookie.
	pairs *pairStore

	// Per-source rate limiter for gating/digipeating decisions — drops
	// over-rate sources so a misbehaving station doesn't get amplified.
	srcLimiter *sourceRateLimiter

	// In-memory counters surfaced on /stats.
	stats *statsCounters
}

type staticAsset struct {
	body []byte
	ct   string
	etag string
}

const recentCap = 200

// recentRFWindow returns the "how recently must we have heard this station on
// RF" window used by both the IS→RF gating decision and the dashboard live-
// feed inclusion rules. Falls back to DefaultIGateRecentRFMinutes (30 min,
// per APRS-IS IGating.aspx convention) when the operator hasn't set it
// explicitly. Configurable on the Settings page in Advanced mode.
func recentRFWindow(snap state.State) time.Duration {
	m := snap.IGateRecentRFMinutes
	if m <= 0 {
		m = state.DefaultIGateRecentRFMinutes
	}
	return time.Duration(m) * time.Minute
}

// mainVersion is set by cmd/aprgo via SetVersion at startup, used for UI display.
var mainVersion = "dev"

// SetVersion lets the main package wire build-time Version in.
func SetVersion(v string) {
	if v != "" {
		mainVersion = v
	}
}

// New constructs the server (does not start listening).
func New(opts Options) (*Server, error) {
	tmplFS, err := fs.Sub(web.Templates, "templates")
	if err != nil {
		return nil, fmt.Errorf("templates: %w", err)
	}
	tmpl, err := template.New("").Funcs(funcMap).ParseFS(tmplFS, "*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	staticSub, err := fs.Sub(web.Static, "static")
	if err != nil {
		return nil, fmt.Errorf("static: %w", err)
	}
	staticMap := map[string]staticAsset{}
	_ = fs.WalkDir(staticSub, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		b, _ := fs.ReadFile(staticSub, path)
		ct := contentTypeFor(path)
		etag := fmt.Sprintf(`"%x"`, sha1Sum(b))
		staticMap[path] = staticAsset{body: b, ct: ct, etag: etag}
		return nil
	})
	log.Printf("static: loaded %d assets", len(staticMap))

	st, err := state.Open(opts.StatePath)
	if err != nil {
		return nil, fmt.Errorf("state: %w", err)
	}
	db, err := store.Open(opts.DBPath)
	if err != nil {
		return nil, fmt.Errorf("db: %w", err)
	}
	b := bus.New()
	s := &Server{
		opts:    opts,
		state:   st,
		auth:    auth.New(st),
		bus:     b,
		store:   db,
		rf:      rf.New(st, b),
		tmpl:    tmpl,
		static:  staticMap,
		// 30s matches APRS-IS reference (javAPRSSrvr) — collapses multi-digi
		// receipts of the same content without swallowing intentional resends.
		dupe:        newDupeTable(30 * time.Second),
		msgDupe:     newDupeTable(30 * time.Second),
		// 15 minutes is wide enough to absorb radios that keep retrying
		// the same body for several minutes even with incrementing msg-ids,
		// while still narrow enough that an *intentional* re-send 15+ min
		// later (e.g. a follow-up after waiting for a response) is treated
		// as a fresh message rather than collapsed.
		msgBodyDupe: newDupeTable(15 * time.Minute),
		recent:     make([]aprs.Packet, 0, recentCap),
		wdrafts:    make(map[string]*wizardDraft),
		loginLimit: newLoginLimiter(),
		pairs:      newPairStore(),
		retries:    newRetryQueue(),
		viscous:    newViscousQueue(),
		drops:      newDropRing(),
		// Per-source rate limit: >30 packets in any 60-second window puts
		// that source in a 15-minute timeout. Designed to silence broken
		// transmitters (stuck PTT, runaway tracker) rather than shape
		// polite traffic — normal stations rarely exceed 5/min.
		srcLimiter: newSourceRateLimiter(30, 15*time.Minute),
		stats:      newStatsCounters(),
		msgTracker:    gate.NewMessagedRecipientTracker(30 * time.Minute),
		bulletinSends: newBulletinSendTracker(),
	}
	s.is = igate.New(st, b)
	s.beacon = beacon.New(st, s.rf, b)
	s.beacon.SetIS(s.is) // enables IS-only beacon transmission for ModeIS
	return s, nil
}

// Run starts the HTTP listener and all background goroutines. Blocks until
// ctx is cancelled or the listener fails. All subsystems are waited on before
// the SQLite store is closed. Each spawned goroutine runs under a recover()
// so a panic logs a stack trace and restarts the goroutine instead of
// crashing the whole binary (which would systemd-flap into StartLimitBurst).
func (s *Server) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	spawn := func(name string, fn func(context.Context)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func(start time.Time) {
				if ctx.Err() != nil {
					log.Printf("shutdown: spawn %s exited after %s", name, time.Since(start).Round(10*time.Millisecond))
				}
			}(time.Now())
			// Capture shutdown start so the exit log reports elapsed since
			// shutdown began, not since program start.
			var shutdownStart time.Time
			for ctx.Err() == nil {
				func() {
					defer func() {
						if r := recover(); r != nil {
							log.Printf("PANIC in %s: %v\n%s", name, r, debug.Stack())
						}
					}()
					fn(ctx)
				}()
				if ctx.Err() != nil {
					if shutdownStart.IsZero() {
						shutdownStart = time.Now()
					}
					return
				}
				select {
				case <-ctx.Done():
					if shutdownStart.IsZero() {
						shutdownStart = time.Now()
					}
					return
				case <-time.After(2 * time.Second):
				}
			}
		}()
	}
	spawn("rf", s.rf.Run)
	// Skip the APRS-IS client when the operator is in Offline mode (no
	// internet). Without this we'd still dial the socket, log endless
	// reconnect failures, and clutter the UI with the "IS disconnected"
	// banner — pointlessly. The is.Send call sites already handle the
	// not-connected case by returning an error, so no other code path
	// needs changes.
	if !s.state.Snapshot().OfflineMode {
		spawn("igate", s.is.Run)
	} else {
		log.Printf("igate: skipped — Offline mode enabled in state")
	}
	spawn("beacon", s.beacon.Run)
	spawn("parseLoop", s.parseLoop)
	spawn("pruner", s.runPruner)
	spawn("wdraftsJanitor", s.wdraftsJanitor)
	spawn("pairsJanitor", s.pairsJanitor)
	spawn("msgRetries", s.runRetryWorker)

	srv := &http.Server{
		Addr:              s.opts.Listen,
		Handler:           s.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	errc := make(chan error, 1)
	go func() {
		log.Printf("listening on %s", s.opts.Listen)
		errc <- srv.ListenAndServe()
	}()
	select {
	case err := <-errc:
		if err != http.ErrServerClosed {
			// Drain background goroutines before returning.
			<-ctx.Done()
			s.viscous.Stop()
			wg.Wait()
			_ = s.store.Close()
			return err
		}
	case <-ctx.Done():
		log.Printf("shutdown: signal received, draining…")
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
		log.Printf("shutdown: http server stopped, waiting for goroutines…")
	}
	// Stop pending fill-in-digi timers so they don't fire against a
	// torn-down RF subsystem during the wg.Wait() window below.
	s.viscous.Stop()
	// Block until all background goroutines have actually exited before closing
	// the SQLite handle — prevents store writes against a closed DB.
	wg.Wait()
	_ = s.store.Close()
	return nil
}

// pairsJanitor prunes stale Bluetooth pair-attempt jobs from the in-flight
// map. Without this, every authenticated user who started a pair attempt
// (or closed the tab mid-pair) leaves a job + cancel closure + 45s timer
// context in memory until process restart — slow leak, exploitable by
// repeated POSTs to /setup/save/tnc.
func (s *Server) pairsJanitor(ctx context.Context) {
	tick := time.NewTicker(5 * time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.pairs.gc()
		}
	}
}

// wdraftsJanitor prunes expired wizard drafts every 5 minutes.
func (s *Server) wdraftsJanitor(ctx context.Context) {
	tick := time.NewTicker(5 * time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.wmu.Lock()
			for k, d := range s.wdrafts {
				if time.Since(d.Modified) > 30*time.Minute {
					delete(s.wdrafts, k)
				}
			}
			s.wmu.Unlock()
		}
	}
}

// parseLoop is the in-process pipeline: subscribe to bus.Frames, parse into
// Packets, store + republish + dispatch gate actions.
func (s *Server) parseLoop(ctx context.Context) {
	sub, cancelSub := s.bus.Frames.Subscribe()
	defer cancelSub()
	for {
		select {
		case <-ctx.Done():
			return
		case f, ok := <-sub:
			if !ok {
				return
			}
			pkt := aprs.Parse(f)
			snap := s.state.Snapshot()
			switch pkt.Frame.Origin {
			case ax25.SrcRF:
				s.stats.pktsRF.Add(1)
			case ax25.SrcIS:
				s.stats.pktsIS.Add(1)
			case ax25.SrcTX:
				s.stats.pktsTX.Add(1)
			}

			// Dupe check (RF only) — drop reflections of our own transmissions
			// and neighbor-digipeated repeats. Key is content (src + dest +
			// info), NOT raw AX.25 bytes, because used-bits and inserted
			// digipeater calls differ between original and neighbor-repeats
			// of the same packet but the content is identical.
			//
			// IMPORTANT: the viscous-queue cancel check runs BEFORE the dupe
			// drop. Otherwise a neighbor's digipeat of our pending content
			// (which we'd discard as a dupe) wouldn't trigger our cancel —
			// our queued TX would still fire 5s later and double-stomp the
			// neighbor's already-aired packet.
			if pkt.Frame.Origin == ax25.SrcRF {
				key := []byte(pkt.Frame.Src + ">" + pkt.Frame.Dest + ":")
				key = append(key, pkt.Frame.Info...)
				if s.viscous.cancelIfQueued(string(key)) {
					suppressedByViscous.Add(1)
					log.Printf("digi: viscous suppressed (heard %s>%s on RF first)", pkt.Frame.Src, pkt.Frame.Dest)
				}
				if s.dupe.CheckAndMark(key) {
					s.stats.dupesDropped.Add(1)
					continue
				}
				// Sanity drop: positions impossibly far from our station are
				// almost always misconfigured trackers (e.g. AnyTone radios
				// beaconing the Guangzhou factory test coordinates until GPS
				// locks). We drop the entire packet — not just the position —
				// so it doesn't pollute the dashboard, heard list, or history.
				// Only applies when both positions are known.
				if (snap.Lat != 0 || snap.Lon != 0) &&
					pkt.Decoded.Lat != nil && pkt.Decoded.Lon != nil &&
					pkt.Decoded.MsgOrigSrc == "" {
					if haversineKm(*pkt.Decoded.Lat, *pkt.Decoded.Lon, snap.Lat, snap.Lon) > maxIntakeDistKm {
						s.stats.distDropped.Add(1)
						continue
					}
				}
				// Per-source rate limit (RF only — IS-origin is filtered
				// upstream by the APRS-IS server). Runs before any storage
				// or dashboard append so a broken transmitter doesn't
				// pollute the heard list, packets table, or live feed.
				// Emits a one-shot diagnostic entry on the packet that
				// trips the threshold; subsequent dropped packets while
				// blocked are silent.
				if ok, justBlocked := s.srcLimiter.Allow(pkt.Frame.Src); !ok {
					if justBlocked {
						s.stats.rateLimited.Add(1)
						s.drops.add(dropEntry{
							Time:   time.Now(),
							Origin: originLabel(pkt.Frame.Origin),
							Source: pkt.Frame.Src,
							Dest:   pkt.Frame.Dest,
							Info:   sanitizeInfo(pkt.Frame.Info),
							Reason: "rate-limited (>30/min, 15-min timeout)",
						})
					}
					continue
				}
			}

			// Dashboard live feed inclusion rules:
			//   RF-origin (heard on the air)              → always show
			//   TX-origin (our own transmissions)         → always show
			//   IS-origin → only if it's a *message* that we care about:
			//     - addressed to our station, OR
			//     - addressed to a station we've heard on RF in the last 2h
			//       (i.e. we're about to IS→RF gate it; operator should see
			//       the IS source line followed by our gated TX line)
			//   This excludes the IS firehose (position beacons, etc.) while
			//   surfacing the IS traffic that actually pairs with on-air
			//   events.
			showOnDashboard := pkt.Frame.Origin != ax25.SrcIS
			if pkt.Frame.Origin == ax25.SrcIS && pkt.Decoded.IsMessage {
				toUs := pkt.Decoded.MsgTo != "" && strings.EqualFold(pkt.Decoded.MsgTo, snap.Callsign)
				toLocal := pkt.Decoded.MsgTo != "" &&
					s.store.HeardOnRF(pkt.Decoded.MsgTo, time.Now().Add(-recentRFWindow(snap)))
				if toUs || toLocal {
					showOnDashboard = true
				}
			}
			if showOnDashboard {
				s.mu.Lock()
				if len(s.recent) == recentCap {
					// Shift in-place to reclaim slot 0 without allocating
					// (the previous `s.recent = s.recent[1:]` slid the
					// header forward and let the backing array grow
					// unbounded over time).
					copy(s.recent, s.recent[1:])
					s.recent = s.recent[:recentCap-1]
				}
				s.recent = append(s.recent, pkt)
				s.recentNextID++
				s.mu.Unlock()
			}

			// Storage policy:
			//
			//   stations table (heard list):
			//     RF-origin from someone other than us. Digipeated copies of
			//     our own beacon arrive RF-origin from us and would otherwise
			//     pollute "who have I heard"; filter them.
			//
			//   packets table (per-station history):
			//     RF-origin from anyone (excluding digi'd copies of our own
			//     beacon, same reason as above) + TX-origin. TX entries are
			//     what makes our own /stations/<self> page useful — every
			//     beacon, every gated IS→RF relay, every digi we did shows
			//     up there with timestamp and full frame.
			//
			//   IS-origin: handled below. Stored only for messages directly
			//     addressed to us; the IS firehose isn't what aprgo is for.
			path := strings.Join(pkt.Frame.Path, ",")
			fromUs := strings.EqualFold(pkt.Frame.Src, snap.Callsign)

			if pkt.Frame.Origin == ax25.SrcRF && !fromUs {
				// Intake-time position validation, applied to the lat/lon
				// we're about to store in the stations table. Three guards:
				//   1. Third-party wrapped packets carry the inner
				//      originator's position; don't credit it to the relay
				//   2. Out-of-range lat/lon (defense; parser should catch)
				//   3. Null-island (~0,0) — usually a cold-GPS tracker
				lat, lon := pkt.Decoded.Lat, pkt.Decoded.Lon
				symbol, comment := pkt.Decoded.Symbol, pkt.Decoded.Comment
				if pkt.Decoded.MsgOrigSrc != "" {
					lat, lon = nil, nil
					symbol, comment = "", ""
				} else if lat != nil && lon != nil {
					if *lat < -90 || *lat > 90 || *lon < -180 || *lon > 180 ||
						(*lat > -0.5 && *lat < 0.5 && *lon > -0.5 && *lon < 0.5) {
						lat, lon = nil, nil
					}
				}
				if err := s.store.UpsertHeard(pkt.Frame.Src, pkt.Frame.RxAt, path, string(pkt.Frame.Info),
					pkt.Frame.Dest, pkt.Frame.Origin, lat, lon, symbol, comment); err != nil {
					log.Printf("store: UpsertHeard %s: %v", pkt.Frame.Src, err)
				}
			}
			if (pkt.Frame.Origin == ax25.SrcRF && !fromUs) || pkt.Frame.Origin == ax25.SrcTX {
				// Cache the parsed position alongside the row so the trails
				// endpoint can build polylines via direct SELECT instead of
				// re-parsing every info field. Apply the same intake guards
				// as the stations table: skip null-island / out-of-range,
				// and (critically) skip positions from third-party wrapped
				// frames — those belong to the inner originator, not the
				// outer source we're indexing under.
				pktLat, pktLon := pkt.Decoded.Lat, pkt.Decoded.Lon
				if pkt.Decoded.MsgOrigSrc != "" {
					pktLat, pktLon = nil, nil
				} else if pktLat != nil && pktLon != nil {
					if *pktLat < -90 || *pktLat > 90 || *pktLon < -180 || *pktLon > 180 ||
						(*pktLat > -0.5 && *pktLat < 0.5 && *pktLon > -0.5 && *pktLon < 0.5) {
						pktLat, pktLon = nil, nil
					}
				}
				if err := s.store.LogPacket(pkt.Frame.RxAt, pkt.Frame.Src, pkt.Frame.Dest, path,
					string(pkt.Frame.Info), pkt.Frame.Origin, pkt.Frame.TNC2(), pktLat, pktLon); err != nil {
					log.Printf("store: LogPacket %s: %v", pkt.Frame.Src, err)
				}
			}

			// Messages logic below stays gated on the original (non-self, non-TX)
			// branch — those are about *conversations*, not transmit history.
			if !fromUs && pkt.Frame.Origin != ax25.SrcTX {
				// Messages: log only messages that are part of THIS operator's
				// conversation. The Messages page is a chat history, not an
				// audit of every relay this iGate considers — gating relay
				// activity is visible on the dashboard live feed (TX badge)
				// and in the packets table for the per-station detail view.
				//
				//   keep if dest == us            → someone messaged us
				//   keep if source == us          → our outbound (defensive)
				//
				// Always drop:
				//   - telemetry config (PARM./UNIT./EQNS./BITS.)
				//   - bulletins (recipient prefix "BLN")
				//   - self-addressed messages (telemetry/status to self)
				//   - everything not involving us as source or dest
				if pkt.Decoded.IsMessage && !pkt.Decoded.IsAck && !pkt.Decoded.IsRej {
					bodyUp := strings.ToUpper(pkt.Decoded.MsgBody)
					isTelemCfg := strings.HasPrefix(bodyUp, "PARM.") ||
						strings.HasPrefix(bodyUp, "UNIT.") ||
						strings.HasPrefix(bodyUp, "EQNS.") ||
						strings.HasPrefix(bodyUp, "BITS.")
					isBulletinDest := isBulletinAddressee(pkt.Decoded.MsgTo)
					isSelfAddressed := strings.EqualFold(pkt.Decoded.MsgTo, pkt.Frame.Src)

					addressedToUs := strings.EqualFold(pkt.Decoded.MsgTo, snap.Callsign)
					fromUs := strings.EqualFold(pkt.Frame.Src, snap.Callsign) ||
						strings.EqualFold(pkt.Decoded.MsgOrigSrc, snap.Callsign)

					// Keep 1:1 messages we're a party to AND all bulletins
					// (BLN*, NWS*, SKY*, CWA-*). Bulletins land in the same
					// messages table; the /bulletins page filters them out
					// of /messages conversations via the existing peer-
					// keyed CTE (which only joins on source=me OR dest=me,
					// so bulletins addressed to BLNxxxx never appear in
					// conversations regardless).
					keep := !isTelemCfg && !isSelfAddressed &&
						(addressedToUs || fromUs || isBulletinDest)

					if keep {
						// Use the third-party original source if present —
						// that's the real "From" (e.g. SMS), not the relay
						// iGate / digipeater that happened to retransmit it.
						source := pkt.Frame.Src
						if pkt.Decoded.MsgOrigSrc != "" {
							source = pkt.Decoded.MsgOrigSrc
						}
						// Dedupe: two windows.
						//   1) (source, dest, body, msg_id) for 60s — collapses
						//      multi-path receipts of the same exact frame.
						//   2) (source, dest, body) for 30s — catches retries
						//      from radios that increment the msg_id on each
						//      retry, which slips past window #1.
						alreadySeen := s.recentlyLogged(source, pkt.Decoded.MsgTo, pkt.Decoded.MsgBody, pkt.Decoded.MsgID) ||
							s.recentlyLoggedBody(source, pkt.Decoded.MsgTo, pkt.Decoded.MsgBody)
						if !alreadySeen {
							_, _ = s.store.LogMessage(store.Message{
								Time:      pkt.Frame.RxAt,
								Direction: "in",
								Source:    source,
								Dest:      pkt.Decoded.MsgTo,
								Body:      pkt.Decoded.MsgBody,
								MsgID:     pkt.Decoded.MsgID,
								ViaRF:     pkt.Frame.Origin == ax25.SrcRF,
								ViaIS:     pkt.Frame.Origin == ax25.SrcIS,
								Raw:       pkt.Frame.TNC2(),
							})
						} else {
							// Same logical message just arrived via the OTHER
							// transport — IS-gated copy beat our RF decode (or
							// vice versa). Merge the via flag onto the existing
							// row so the messages page reflects "heard via both."
							_ = s.store.MergeMessageVia(source, pkt.Decoded.MsgTo, pkt.Decoded.MsgBody,
								pkt.Frame.Origin == ax25.SrcRF,
								pkt.Frame.Origin == ax25.SrcIS)
						}

						// Auto-ACK: if the message has a msg-id AND was
						// addressed to us, reply with `:THEIR-CALL:ackXXX`
						// over the same medium it arrived on. Without this
						// the sender's radio thinks we never heard them and
						// retries 3-5 times, cluttering the conversation.
						// Don't ack third-party / origSrc relayed messages
						// — the relay iGate should ack those.
						if addressedToUs && pkt.Decoded.MsgID != "" && pkt.Decoded.MsgOrigSrc == "" {
							s.sendAck(pkt.Frame.Src, pkt.Decoded.MsgID, pkt.Frame.Origin, snap.Callsign)
						}
					}
				}
				if pkt.Decoded.IsAck {
					// MarkAck returns the message ID it just flipped so we
					// can yank it from the retry queue immediately — without
					// this the retry worker might fire a final retransmit
					// after the ack already landed (cosmetic, but ugly).
					id, _ := s.store.MarkAck(pkt.Decoded.MsgTo, pkt.Frame.Src, pkt.Decoded.AckedID)
					if id > 0 {
						s.retries.Remove(id)
					}
				}
				if pkt.Decoded.IsRej {
					// Peer refused the message. Stop retrying (same as ack
					// at the protocol level) but mark the row as rejected so
					// the UI can distinguish delivered from refused.
					id, _ := s.store.MarkRej(pkt.Decoded.MsgTo, pkt.Frame.Src, pkt.Decoded.AckedID)
					if id > 0 {
						s.retries.Remove(id)
					}
				}
				if pkt.Decoded.ReplyAckID != "" && pkt.Decoded.IsMessage {
					// APRS 1.1 piggyback ack — the inbound message also acks
					// our prior outgoing msgID. Dequeue retries the same way
					// a standalone ack does. Source/dest are swapped relative
					// to a normal ack: pkt.Frame.Src is the peer, pkt.Decoded.MsgTo
					// is us.
					id, _ := s.store.MarkAck(pkt.Decoded.MsgTo, pkt.Frame.Src, pkt.Decoded.ReplyAckID)
					if id > 0 {
						s.retries.Remove(id)
					}
				}
			}

			if showOnDashboard {
				s.bus.Packets.Publish(pkt)
			}

			heardOnRF := func(call string) bool {
				return s.store.HeardOnRF(call, time.Now().Add(-recentRFWindow(snap)))
			}
			// Answer ?IGATE? / ?APRS? general queries (RF-side only, per spec).
			s.maybeAnswerQuery(ctx, pkt, snap)
			for _, a := range gate.Decide(pkt, snap, heardOnRF, s.msgTracker) {
				switch a.Kind {
				case gate.SendIS:
					if err := s.is.Send(a.Payload); err != nil {
						log.Printf("gate: SendIS failed: %v", err)
					} else {
						s.stats.sentIS.Add(1)
					}
				case gate.SendRF:
					if a.Viscous {
						// Hold + listen; cancel from parseLoop if same
						// content shows up on RF inside the window.
						s.enqueueViscous(a, pkt)
					} else if err := s.dispatchSendRF(a); err != nil {
						if !errors.Is(err, rf.ErrTXDisabled) {
							log.Printf("gate: SendRF failed: %v (%s)", err, a.Reason)
						}
					} else {
						s.stats.sentRF.Add(1)
						if strings.HasPrefix(a.Reason, "digi ") || strings.HasPrefix(a.Reason, "preempt") {
							s.stats.digipeats.Add(1)
						} else if strings.HasPrefix(a.Reason, "IS→RF msg→") {
							s.stats.igateMsgsRF.Add(1)
						}
						log.Printf("gate: TX %s", a.Reason)
					}
				case gate.Drop:
					s.stats.recordDropReason(a.Reason)
					s.drops.add(dropEntry{
						Time:   time.Now(),
						Origin: originLabel(pkt.Frame.Origin),
						Source: pkt.Frame.Src,
						Dest:   pkt.Frame.Dest,
						Info:   sanitizeInfo(pkt.Frame.Info),
						Reason: a.Reason,
					})
				}
			}
		}
	}
}

func (s *Server) runPruner(ctx context.Context) {
	timer := time.NewTimer(2 * time.Minute)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		days := s.state.Snapshot().RetentionDays
		if days > 0 {
			cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
			if n, err := s.store.Prune(cutoff); err != nil {
				log.Printf("prune: %v", err)
			} else if n > 0 {
				log.Printf("prune: deleted %d rows older than %d days", n, days)
			}
		}
		timer.Reset(1 * time.Hour)
	}
}

// recentlyLogged returns true if a message with the same (source, dest, body,
// msgID) tuple has been logged within the dedupe window. Used to collapse
// multi-path repeats of one message into a single log entry.
func (s *Server) recentlyLogged(source, dest, body, msgID string) bool {
	key := []byte(strings.ToUpper(source) + ">" + strings.ToUpper(dest) + "|" + msgID + "|" + body)
	return s.msgDupe.CheckAndMark(key)
}

// recentlyLoggedBody is the no-msgID fallback dupe check. Returns true iff
// we've logged a message with the same (source, dest, body) in the last 30s.
// Catches retries from radios that bump the msg_id every retry.
func (s *Server) recentlyLoggedBody(source, dest, body string) bool {
	key := []byte(strings.ToUpper(source) + ">" + strings.ToUpper(dest) + "|" + body)
	return s.msgBodyDupe.CheckAndMark(key)
}

// sendAck transmits an APRS ack for `theirMsgID` back to `theirCall` via
// whichever medium the original message arrived on (RF or IS). The synthesized
// frame is published through the standard SrcTX dispatcher so it shows up on
// the dashboard live feed in amber and in the per-station TX history.
func (s *Server) sendAck(theirCall, theirMsgID string, origin ax25.Source, myCall string) {
	if theirCall == "" || theirMsgID == "" || myCall == "" {
		return
	}
	info := aprs.AckInfo(theirCall, theirMsgID)
	dstCall := state.DefaultBeaconDest // tocall — same as outbound messages
	switch origin {
	case ax25.SrcRF:
		if err := s.txRF(myCall, dstCall, []string{"WIDE1-1"}, []byte(info)); err != nil {
			log.Printf("ack: rf %s/%s: %v", theirCall, theirMsgID, err)
		}
	case ax25.SrcIS:
		if err := s.txIS(myCall, dstCall, []byte(info)); err != nil {
			log.Printf("ack: is %s/%s: %v", theirCall, theirMsgID, err)
		}
	default:
		// SrcTX shouldn't happen here; if it does, no-op.
	}
}

// txRF encodes a UI frame, transmits it via the RF TNC, marks the dupe
// table so the inevitable RF echo of our own TX doesn't loop back through
// parseLoop as a fresh receive, and publishes a synthesized SrcTX frame
// so the dashboard's live feed shows the transmission. ANY code path
// originating an outbound RF frame outside of the gate (user-initiated
// /messages/send, auto-ACK, message retry) should use this instead of
// calling s.rf.TX directly — otherwise the dashboard misses it.
func (s *Server) txRF(src, dst string, path []string, info []byte) error {
	built, err := ax25.EncodeUIFrame(src, dst, path, info)
	if err != nil {
		return err
	}
	if frame, ferr := ax25.FromAX25(built, ax25.SrcRF, "rf"); ferr == nil {
		key := []byte(frame.Src + ">" + frame.Dest + ":")
		key = append(key, frame.Info...)
		s.dupe.CheckAndMark(key)
	}
	if err := s.rf.TX(built); err != nil {
		return err
	}
	if frame, ferr := ax25.FromAX25(built, ax25.SrcTX, "rf"); ferr == nil {
		s.bus.Frames.Publish(frame)
	}
	return nil
}

// txIS sends a TNC2-formatted packet up to APRS-IS and publishes a synth
// SrcTX frame so the dashboard's TX-filter chip surfaces it. Caller is
// responsible for the TNC2 framing (`src>dst,path:info`).
func (s *Server) txIS(src, dst string, info []byte) error {
	pkt := fmt.Sprintf("%s>%s,TCPIP*:%s", src, dst, info)
	if err := s.is.Send(pkt); err != nil {
		return err
	}
	// Synthesize a Frame for the dashboard view. No real path on the wire
	// (TCPIP* is virtual), so we leave Path empty.
	frame := ax25.Frame{
		Src: src, Dest: dst, Info: info, Path: nil,
		Origin: ax25.SrcTX, RxAt: time.Now(),
	}
	s.bus.Frames.Publish(frame)
	return nil
}

// enqueueViscous holds a fill-in WIDE1-1 digipeat for a randomized 3–5 s
// politeness window. If we hear the same content on RF during the window
// (a higher-elevation digi handled it before us), the parseLoop's call to
// viscous.cancelIfQueued() drops the queued TX. Otherwise the timer fires
// and we hand off to dispatchSendRF like any other TX.
//
// Content hash matches the same key used by the dupe table so a cancel
// triggers regardless of which digi did the work or how the path differs.
func (s *Server) enqueueViscous(a gate.Action, pkt aprs.Packet) {
	key := pkt.Frame.Src + ">" + pkt.Frame.Dest + ":" + string(pkt.Frame.Info)
	queued := s.viscous.enqueue(key, a, func(action gate.Action) {
		if err := s.dispatchSendRF(action); err != nil {
			if !errors.Is(err, rf.ErrTXDisabled) {
				log.Printf("gate: viscous SendRF failed: %v (%s)", err, action.Reason)
			}
		} else {
			log.Printf("gate: TX %s (viscous fired — no other digi handled it)", action.Reason)
		}
	})
	if queued {
		log.Printf("gate: %s queued viscous (3–5s hold)", a.Reason)
	}
}

// dispatchSendRF takes a gate.SendRF Action and pushes it onto the rf TX queue.
// Either RFRaw is set (re-TX existing bytes, used by digipeat) or the
// RFSrc/RFDest/RFPath/RFInfo fields are set (build a fresh frame).
// On success also marks the dupe table so we don't reflect our own TX, and
// publishes a synthesized TX packet to bus.Packets so the dashboard shows it.
func (s *Server) dispatchSendRF(a gate.Action) error {
	var raw []byte
	if a.RFRaw != nil {
		raw = a.RFRaw
	} else {
		built, err := ax25.EncodeUIFrame(a.RFSrc, a.RFDest, a.RFPath, a.RFInfo)
		if err != nil {
			return err
		}
		raw = built
	}
	// Pre-mark in the dupe table so the inevitable RF echo of our own TX is
	// suppressed. Key on content (src+dest+info) to match parseLoop's policy.
	if frame, ferr := ax25.FromAX25(raw, ax25.SrcRF, "rf"); ferr == nil {
		key := []byte(frame.Src + ">" + frame.Dest + ":")
		key = append(key, frame.Info...)
		s.dupe.CheckAndMark(key)
	}
	if err := s.rf.TX(raw); err != nil {
		return err
	}
	// Surface the TX on the dashboard by publishing as a SrcTX Frame; parseLoop
	// will pick it up and broadcast on bus.Packets like everything else.
	if frame, ferr := ax25.FromAX25(raw, ax25.SrcTX, "rf"); ferr == nil {
		s.bus.Frames.Publish(frame)
	}
	return nil
}

func (s *Server) recentPackets() []aprs.Packet {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]aprs.Packet, len(s.recent))
	copy(out, s.recent)
	return out
}

// feedMaxAge bounds the dashboard live feed by time as well as by buffer
// size. The ring buffer holds up to 200 packets regardless of age, but the
// dashboard only ever surfaces ones from the last feedMaxAge window. So
// "live" actually means live: a quiet band that produced one packet two
// hours ago doesn't keep displaying that stale packet forever.
const feedMaxAge = 30 * time.Minute

// recentSince returns all in-buffer packets newer than `cursor` AND newer
// than feedMaxAge (along with each one's monotonic ID and the current head
// cursor). Used by the dashboard poll endpoint.
//
// If cursor is 0 (initial page load) the client gets back the full
// in-window buffer; if cursor is older than what we still have buffered,
// the client missed packets — they get whatever survives and a fresh
// cursor.
func (s *Server) recentSince(cursor uint64) ([]aprs.Packet, []uint64, uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	head := s.recentNextID
	if len(s.recent) == 0 {
		return nil, nil, head
	}
	// Compute the ID of recent[0]: head minus the size of the window.
	base := head - uint64(len(s.recent)) + 1
	// Cap the cursor at `base - 1` so old cursors return the full window.
	if cursor+1 < base {
		cursor = base - 1
	}
	startIdx := int(cursor - base + 1)
	if startIdx < 0 {
		startIdx = 0
	}
	if startIdx >= len(s.recent) {
		return nil, nil, head
	}
	// Age filter — drop anything older than feedMaxAge. We walk forward
	// from startIdx and only keep packets whose RxAt is within the window.
	// Note: packets in the ring are appended in chrono order, but we still
	// scan because a single very-quiet period might leave the buffer
	// holding mostly-old entries.
	cutoff := time.Now().Add(-feedMaxAge)
	pkts := make([]aprs.Packet, 0, len(s.recent)-startIdx)
	ids := make([]uint64, 0, len(s.recent)-startIdx)
	for i := startIdx; i < len(s.recent); i++ {
		if s.recent[i].Frame.RxAt.Before(cutoff) {
			continue
		}
		pkts = append(pkts, s.recent[i])
		ids = append(ids, base+uint64(i))
	}
	return pkts, ids, head
}

func (s *Server) serveStatic(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/static/")
	a, ok := s.static[path]
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", a.ct)
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Header().Set("ETag", a.etag)
	if match := r.Header.Get("If-None-Match"); match == a.etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(a.body)))
	_, _ = w.Write(a.body)
}

func contentTypeFor(name string) string {
	switch {
	case strings.HasSuffix(name, ".css"):
		return "text/css; charset=utf-8"
	case strings.HasSuffix(name, ".js"):
		return "application/javascript; charset=utf-8"
	case strings.HasSuffix(name, ".png"):
		return "image/png"
	case strings.HasSuffix(name, ".svg"):
		return "image/svg+xml"
	case strings.HasSuffix(name, ".woff2"):
		return "font/woff2"
	default:
		return "application/octet-stream"
	}
}

func sha1Sum(b []byte) []byte { h := sha1.Sum(b); return h[:] }

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, "template error: "+err.Error(), http.StatusInternalServerError)
	}
}

// common returns the standard template context populated with auth / theme / etc.
// `r` is needed so we can derive the per-session CSRF token; pass nil for
// pre-login pages (login itself) where CSRF tokens aren't applicable.
func (s *Server) common(title string, r *http.Request) map[string]any {
	snap := s.state.Snapshot()
	isErr, isErrAt := s.is.LastError()
	rfErr, rfErrAt := s.rf.LastError()
	data := map[string]any{
		"Title":            title,
		"Authed":           true,
		"DefaultPassword":  s.state.IsDefaultPassword(),
		"Theme":            snap.Theme,
		"TZ":               snap.Timezone,
		"TF":               snap.TimeFormat,
		"ServerTZ":         time.Local.String(),
		"CustomTZ":         snap.Timezone != "" && !isCuratedTZ(snap.Timezone),
		"St":               snap,
		"RFConnected":      s.rf.Connected(),
		"RFNotKISS":        s.rf.NotKISSDetected(),
		"ISConnected":      s.is.Connected(),
		"ISVerified":       s.is.Verification() == igate.VerificationVerified,
		"ISUnverified":     s.is.Verification() == igate.VerificationUnverified,
		"ISLastError":      isErr,
		"ISLastErrorAt":    isErrAt,
		"RFLastError":      rfErr,
		"RFLastErrorAt":    rfErrAt,
		"TXDisabled":       !snap.TXEnable && snap.TNCKind != state.TNCNone,
		"Version":          mainVersion,
	}
	if r != nil {
		if c, err := r.Cookie("aprgo_session"); err == nil {
			data["CSRFToken"] = s.csrfTokenFor(c.Value)
		}
	}
	return data
}

// resolveTZ returns the *time.Location for the configured timezone string.
// Empty / invalid falls back to time.Local. Cached for the common case where
// the same name is looked up many times per render cycle.
var (
	tzCacheMu sync.RWMutex
	tzCache   = map[string]*time.Location{}
)

// curatedTZ matches the dropdown options in settings.html — kept here so
// the template can render a "Custom" optgroup when the operator's saved
// value isn't one of the curated entries (e.g. set via state.json directly).
var curatedTZ = map[string]struct{}{
	"UTC":                 {},
	"America/New_York":    {},
	"America/Chicago":     {},
	"America/Denver":      {},
	"America/Phoenix":     {},
	"America/Los_Angeles": {},
	"America/Anchorage":   {},
	"America/Honolulu":    {},
	"America/Toronto":     {},
	"America/Mexico_City": {},
	"America/Sao_Paulo":   {},
	"Europe/London":       {},
	"Europe/Paris":        {},
	"Europe/Berlin":       {},
	"Europe/Madrid":       {},
	"Europe/Rome":         {},
	"Europe/Amsterdam":    {},
	"Europe/Stockholm":    {},
	"Europe/Helsinki":     {},
	"Europe/Athens":       {},
	"Europe/Moscow":       {},
	"Africa/Cairo":        {},
	"Africa/Johannesburg": {},
	"Asia/Dubai":          {},
	"Asia/Kolkata":        {},
	"Asia/Bangkok":        {},
	"Asia/Singapore":      {},
	"Asia/Hong_Kong":      {},
	"Asia/Tokyo":          {},
	"Asia/Seoul":          {},
	"Australia/Perth":     {},
	"Australia/Sydney":    {},
	"Pacific/Auckland":    {},
}

func isCuratedTZ(name string) bool {
	_, ok := curatedTZ[name]
	return ok
}

func resolveTZ(name string) *time.Location {
	if name == "" {
		return time.Local
	}
	tzCacheMu.RLock()
	loc, ok := tzCache[name]
	tzCacheMu.RUnlock()
	if ok {
		return loc
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return time.Local
	}
	tzCacheMu.Lock()
	tzCache[name] = loc
	tzCacheMu.Unlock()
	return loc
}

// clockLayout / datetimeLayout return Go time-format strings for the
// operator's chosen 12h/24h preference.
func clockLayout(tf string) string {
	if tf == "12h" {
		return "3:04:05 PM"
	}
	return "15:04:05"
}
func datetimeLayout(tf string) string {
	if tf == "12h" {
		return "2006-01-02 3:04:05 PM"
	}
	return "2006-01-02 15:04:05"
}

// funcMap is template helpers.
var funcMap = template.FuncMap{
	"timeFormat": func(t time.Time, layout string) string { return t.Format(layout) },
	"fmtClock": func(t time.Time, tz string, tf string) string {
		return t.In(resolveTZ(tz)).Format(clockLayout(tf))
	},
	"fmtDateTime": func(t time.Time, tz string, tf string) string {
		return t.In(resolveTZ(tz)).Format(datetimeLayout(tf))
	},
	"ago": func(t time.Time) string {
		d := time.Since(t).Round(time.Second)
		switch {
		case d < time.Minute:
			return fmt.Sprintf("%ds", int(d.Seconds()))
		case d < time.Hour:
			return fmt.Sprintf("%dm", int(d.Minutes()))
		case d < 24*time.Hour:
			return fmt.Sprintf("%dh", int(d.Hours()))
		default:
			return fmt.Sprintf("%dd", int(d.Hours()/24))
		}
	},
	"join":   strings.Join,
	"callsignBase": func(s string) string {
		if i := strings.IndexByte(s, '-'); i >= 0 {
			return s[:i]
		}
		return s
	},
	"callsignSSID": func(s string) string {
		if i := strings.IndexByte(s, '-'); i >= 0 {
			return s[i+1:]
		}
		return "0"
	},
	"intSeq": func(lo, hi int) []int {
		if hi < lo {
			return nil
		}
		out := make([]int, 0, hi-lo+1)
		for n := lo; n <= hi; n++ {
			out = append(out, n)
		}
		return out
	},
	"unix":   func(t time.Time) int64 { return t.Unix() },
	"add":    func(a, b int) int { return a + b },
	// telemEqn formats an APRS telemetry channel equation "y = a·x² + b·x + c"
	// in a human-readable form, dropping zero terms so common simple
	// linear conversions (most stations use just b≠0) read cleanly.
	"telemEqn": func(coeffs [5][3]float64, idx int) string {
		a, b, c := coeffs[idx][0], coeffs[idx][1], coeffs[idx][2]
		if a == 0 && b == 0 && c == 0 {
			return ""
		}
		parts := []string{}
		if a != 0 {
			parts = append(parts, fmt.Sprintf("%gx²", a))
		}
		if b != 0 {
			if len(parts) > 0 && b > 0 {
				parts = append(parts, fmt.Sprintf("+ %gx", b))
			} else if b == 1 {
				parts = append(parts, "x")
			} else {
				parts = append(parts, fmt.Sprintf("%gx", b))
			}
		}
		if c != 0 {
			if len(parts) > 0 && c > 0 {
				parts = append(parts, fmt.Sprintf("+ %g", c))
			} else {
				parts = append(parts, fmt.Sprintf("%g", c))
			}
		}
		return strings.Join(parts, " ")
	},
	"div": func(a, b int) int {
		if b == 0 {
			return 0
		}
		return a / b
	},
	"float": func(n int) float64 { return float64(n) },
	"divf": func(a, b float64) float64 {
		if b == 0 {
			return 0
		}
		return a / b
	},
	"dur": func(d time.Duration) string {
		d = d.Round(time.Second)
		days := int(d / (24 * time.Hour))
		d -= time.Duration(days) * 24 * time.Hour
		hours := int(d / time.Hour)
		d -= time.Duration(hours) * time.Hour
		mins := int(d / time.Minute)
		switch {
		case days > 0:
			return fmt.Sprintf("%dd %dh %dm", days, hours, mins)
		case hours > 0:
			return fmt.Sprintf("%dh %dm", hours, mins)
		default:
			return fmt.Sprintf("%dm", mins)
		}
	},
	"comma": func(n any) string {
		var v int64
		switch x := n.(type) {
		case int:
			v = int64(x)
		case int64:
			v = x
		case uint64:
			v = int64(x)
		default:
			return fmt.Sprint(n)
		}
		s := fmt.Sprintf("%d", v)
		neg := false
		if v < 0 {
			neg = true
			s = s[1:]
		}
		var b strings.Builder
		for i, c := range s {
			if i > 0 && (len(s)-i)%3 == 0 {
				b.WriteByte(',')
			}
			b.WriteRune(c)
		}
		if neg {
			return "-" + b.String()
		}
		return b.String()
	},
	"oneOf": func(v string, opts ...string) bool {
		for _, o := range opts {
			if v == o {
				return true
			}
		}
		return false
	},
	"dict": func(kv ...any) map[string]any {
		m := make(map[string]any, len(kv)/2)
		for i := 0; i+1 < len(kv); i += 2 {
			if k, ok := kv[i].(string); ok {
				m[k] = kv[i+1]
			}
		}
		return m
	},
	"eqByte": func(b byte, s string) bool { return len(s) > 0 && s[0] == b },
	"aprsIcon":      func(sym string) template.HTML { return renderAPRSIcon(sym, "") },
	"aprsIconLarge": func(sym string) template.HTML { return renderAPRSIcon(sym, "large") },
}

// renderAPRSIcon emits the HTML for an APRS symbol from the hessu/aprs-symbols
// 48px sprite sheets, rendered at 24 logical pixels (default) or 48 logical
// pixels (size="large", for hero contexts like the station-detail header).
func renderAPRSIcon(sym, size string) template.HTML {
	if len(sym) < 2 {
		return ""
	}
	t, c := sym[0], sym[1]
	if c < 0x21 || c > 0x7e {
		return ""
	}
	sprite := "1"
	if t == '/' {
		sprite = "0"
	}
	idx := int(c) - 0x21
	x := -((idx % 16) * 24)
	y := -((idx / 16) * 24)
	cls := "aprs-sym"
	wrapCls := "aprs-sym-wrap"
	if size == "large" {
		cls += " large"
		wrapCls += " large"
	}
	html := fmt.Sprintf(
		`<span class="%s" title="%s" style="background-image:url(/static/aprs-symbols-48-%s.png);background-position:%dpx %dpx"></span>`,
		cls, template.HTMLEscapeString(sym), sprite, x, y,
	)
	if t != '/' && t != '\\' {
		oc := int(t) - 0x21
		if oc >= 0 && oc < 16*6 {
			ox := -((oc % 16) * 24)
			oy := -((oc / 16) * 24)
			html = fmt.Sprintf(
				`<span class="%s" title="%s"><span class="aprs-sym" style="background-image:url(/static/aprs-symbols-48-%s.png);background-position:%dpx %dpx"></span><span class="aprs-sym-overlay" style="background-image:url(/static/aprs-symbols-48-2.png);background-position:%dpx %dpx"></span></span>`,
				wrapCls, template.HTMLEscapeString(sym), sprite, x, y, ox, oy,
			)
		}
	}
	return template.HTML(html)
}

// maxIntakeDistKm is the great-circle distance beyond which an inbound RF
// packet's claimed position is considered bogus (almost always a
// misconfigured tracker — e.g. AnyTone radios beacon the Guangzhou factory
// coordinates 23N/113E until GPS acquires). The packet is dropped entirely.
// No setting: 500 km comfortably exceeds any plausible RF reception range
// (typical APRS is <200 km even with terrain-favorable paths).
const maxIntakeDistKm = 500.0

// haversineKm returns the great-circle distance in km between two lat/lon
// pairs (degrees). Accurate enough for the 500 km sanity check; no need
// for the more expensive Vincenty formulation.
func haversineKm(lat1, lon1, lat2, lon2 float64) float64 {
	const R = 6371.0
	rad := math.Pi / 180
	dLat := (lat2 - lat1) * rad
	dLon := (lon2 - lon1) * rad
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*rad)*math.Cos(lat2*rad)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return R * 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
}

