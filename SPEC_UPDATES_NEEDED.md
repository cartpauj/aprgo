# APRS Spec Compliance Audit — aprgo

Eleven verification passes over the codebase against APRS official documentation. List stabilized at 33 findings (passes 10 and 11 confirmed no reversals or new material items).

Spec sources consulted: APRS101.PDF, aprs.org aprs11/aprs12 addenda (fix14439, preemptive-digipeating, RFlimits, SSIDs, mic-e-types, spec-wx, datum, replyacks), aprs-is.net (IGating, IGateDetails, q, Connecting), he.fi base-91 telemetry, AX.25 v2.2.

---

## HIGH (4)

### 1. IS→RF re-gates third-party-wrapped messages ✅ DONE
**File:** `internal/gate/gate.go:309-367` (`decideFromIS`)
`decideFromRF` blocks `}` info-byte at gate.go:134; `decideFromIS` does not. `aprs.Decode` recursively unwraps `}SRC>DEST::ADDR    :body` and populates `IsMessage` / `MsgTo` / `MsgOrigSrc`, so the inner message satisfies the IS→RF gate path. We then re-wrap and transmit `}OUTER>DEST,TCPIP*,MYCALL*:}INNER...` (nested third-party). Loop/amplification risk.
**Fix:** at top of `decideFromIS`, reject if `len(p.Frame.Info) > 0 && p.Frame.Info[0] == '}'` (mirror RF→IS guard). Add gate_test coverage.

### 2. Object decoder uses wrong byte offset ✅ DONE
**File:** `internal/aprs/info.go:121`
Object format (APRS101 §11) is `;NNNNNNNNN*HHMMSSz<position>` = 1 (`;`) + 9 (name) + 1 (live/kill `*` or `_`) + 7 (timestamp) = 18 bytes before position. Code slices `info[11:]` and passes the timestamp+position to `decodeUncompressedOrCompressed`. Lat/lon parse from the timestamp bytes — every Object packet silently decodes garbage or fails.
**Fix:** `decodeUncompressedOrCompressed(info[18:], &d)`; require `len(info) > 18`; surface live/kill flag from `info[10]`.

### 3. `ack` and `rej` both set `IsAck=true` ✅ DONE
**File:** `internal/aprs/info.go:715-718`
Both ack and rej branches set `d.IsAck = true` and write `AckedID`. There is no `IsRej` field on `Decoded`, so a rejected message is indistinguishable from an acknowledged one downstream. Retry logic will mark rejected sends as successfully delivered.
**Fix:** add `IsRej bool` on `Decoded`; set on the rej branch.

### 4. `MsgID = LastIndex("{")` breaks reply-ack form ✅ DONE
**File:** `internal/aprs/info.go:720`
APRS 1.1 reply-ack form (aprs11/replyacks.txt) is `body{II}AA` — own msgID `II` followed by `}AA` piggyback ack. Code uses `LastIndex("{")` then `body[i+1:]` which leaves `}AA` glued onto MsgID. Downstream cannot match our outgoing msgID to the inbound `}AA` reference.
**Fix:** after locating the `{`, additionally split on the next `}` — left side is own msgID, right side is the piggyback ack to credit against the peer's outstanding sends.

---

## MED (21)

### 5. APRS-IS reconnect backoff starts too low ✅ DONE
**File:** `internal/igate/igate.go:114`
Initial backoff = 5s. APRS-IS Connecting.aspx asks ≥30s between reconnects (connect-storm avoidance).
**Fix:** start backoff at 30s.

### 6. HeardOnRF lookup is case-sensitive ✅ DONE
**File:** `internal/store/store.go:350`
SQLite default `=` comparison; message addressees in mixed case miss the heard row.
**Fix:** `UPPER(callsign) = UPPER(?)` plus normalize at insert; or declare `callsign TEXT COLLATE NOCASE`.

### 7. `DefaultIGateRecentRFMinutes = 360` ✅ DONE
**File:** `internal/state/state.go:171`
6 hours; IGating.aspx recommends 30 min (sometimes 60). Six hours generates spurious IS→RF traffic for stations long out of range.
**Fix:** default 30.

### 8. Dupe window is 60s ✅ DONE
**File:** `internal/server/server.go:177-178`
APRS-IS reference (javAPRSSrvr) uses 30s. 60s is over-conservative.
**Fix:** 30s for content dupe; keep msgBodyDupe at 15 min for retry collapsing.

### 9. No "gate next position of messaged-to sender" ✅ DONE
**File:** `internal/gate/gate.go decideFromIS`
Per IGating.aspx, after IS→RF gating a message, the iGate should pass the next position packet seen for the original sender so the RF side learns the responder's location.
**Fix:** add TTL map (e.g. 30 min) of messaged-to senders; in `decideFromIS` permit one position-packet gate per entry; consume after one use.

### 10. Always emits `qAR` ✅ DONE
**File:** `internal/gate/gate.go:382`
RX-only / `Passcode=="-1"` / `!GateIStoRF` iGates should emit `qAO` per q.aspx.
**Fix:** branch on TX capability: `qAR` only when we can actually gate IS→RF, else `qAO`.

### 11. AX.25 decode does not validate callsign charset ✅ DONE
**File:** `internal/ax25/ax25.go:155-167`
`decodeCallsign` shifts bytes and trims spaces; no `[A-Z0-9]` check. Junk bytes from RF noise enter heard list, dupe table, IS gate.
**Fix:** reject in `DecodeUIFrame` if any decoded address contains chars outside `[A-Z0-9]`.

### 12. Beacon comment sanitization too loose ✅ DONE
**File:** `internal/beacon/beacon.go:150` and configuration save
Only `\r`/`\n` stripped. APRS101 §5 reserves `|` and `~` (telemetry/future). No length cap allows the AX.25 info field to balloon.
**Fix:** strip all control chars (0x00–0x1F, 0x7F) and `|`/`~`; cap comment length (~36 chars for position).

### 13. KISS TXDELAY / PERSIST / SLOTTIME / TXTAIL never sent on connect ✅ DONE
**File:** `internal/rf/rf.go` (no emission of KISS commands 0x01–0x04 anywhere)
Default TXDelay on many TNCs is too short for slow-keying radios (lost preamble). No PERSIST/SLOTTIME CSMA tuning.
**Fix:** on session open, send `c0 01 <delay/10ms> c0` etc. using configurable `state.TXDelayMs` (default 300), Persist, SlotTime.

### 14. No `?IGATE?` query response ✅ DONE
APRS101 query mechanism — iGate should reply to `?IGATE?` with capability beacon `<IGATE,MSG_CNT=n,LOC_CNT=m`.
**Fix:** implement query handler; emit periodic capability beacon.

### 15. Compressed-position cs / T-byte altitude not decoded ✅ DONE
**File:** `internal/aprs/info.go:432-456`
Bytes `s[10..11]` (cs) and `s[12]` (T-byte) are ignored. Compressed-position altitude (when T-byte bits indicate altitude) and course/speed both missed.
**Fix:** decode cs by T-byte bits 3-4: course/speed, range, or altitude.

### 16. Mic-E comment altitude `xxx}` not decoded ✅ DONE
**File:** `internal/aprs/info.go:637-647`
Three base-91 chars immediately preceding `}` encode `(c0-33)*91² + (c1-33)*91 + (c2-33) − 10000` meters. Code strips them as part of the comment.
**Fix:** detect and decode before stripping.

### 17. Positionless WX `_MMDDHHMM...` not handled ✅ DONE
**File:** `internal/aprs/info.go:110-139`
No `case '_'` in DTI switch. Weather-only beacons completely ignored.
**Fix:** add positionless WX decoder per APRS101 §12.5.

### 18. Mic-E lonMin codes 60-69 silently wrap to 0-9 ✅ DONE
**File:** `internal/aprs/info.go:574-577`
Per mic-e-types.txt, body[1]-28 values 60-69 are reserved/invalid and the packet should be rejected. Code silently subtracts 60 and accepts.
**Fix:** reject the packet when lonMin ≥ 60.

### 19. Weather wind regex unanchored ✅ DONE
**File:** `internal/aprs/weather.go:56`
`weatherWindRE = (\d{3})/(\d{3})` matches anywhere in comment. False positives on non-`_` symbols with stray digit patterns.
**Fix:** anchor `^` or require comment offset 0 only when symbol code is `_`.

### 20. Outbound message body no 67-char cap ✅ DONE
**File:** `internal/aprs/message.go`
APRS101 §14 caps body at 67 chars; over-length lines get dropped by some IS hubs.
**Fix:** clamp or reject at compose-time.

### 21. No base-91 `|...|` telemetry decode ✅ DONE
**File:** `internal/aprs/info.go:660`
he.fi/aprs-base91-comment-telemetry.txt defines `|ss11223344|` comment-embedded telemetry; common in modern trackers. Only `T#` form is parsed.
**Fix:** scan position comments for `|`-delimited base-91 telemetry block.

### 22. IS→RF emits with `RFPath: nil` ✅ DONE
**File:** `internal/gate/gate.go:363`
Outer RF frame has no via path. Convention is operator-configurable (typically `WIDE1-1`).
**Fix:** add `state.IGateTXPath` (default `WIDE1-1`); pass through as `RFPath`.

### 23. Beacon-path regex accepts WIDE1-0 and WIDE1-2 ✅ DONE
**File:** `internal/server/sanitize.go:38`
`^WIDE([12])-([012])$` allows invalid `WIDE1-0` (already-used) and `WIDE1-2` (undefined in New-N — fill-in is single-hop only).
**Fix:** `^(WIDE1-1|WIDE2-[12])$`.

### 24. Wizard accepts unvalidated callsign / passcode ✅ DONE
**File:** `internal/server/wizard.go:221-222`
Plain `TrimSpace` only; lowercase, garbage strings, non-numeric passcodes all flow through.
**Fix:** apply `validateCallsign` (and `ToUpper`); require passcode numeric (or `-1`) and ≤32767.

### 25. Outgoing msgID collision risk ✅ DONE
**File:** `internal/server/routes.go:947`
`fmt.Sprintf("%03d", (ms+rb)%1000)` — three-digit IDs are spec-correct but there's no check against currently-pending IDs; two sends within a ms collide and an ack to either resolves both.
**Fix:** allocate from a pending-ID set; retry until free.

### 26. Outbound message body not filtered for reserved chars ✅ DONE
**File:** `internal/server/sanitize.go` / `internal/aprs/message.go`
APRS101 §14 forbids `{` (msgID delimiter), `|`, `~` (reserved). User-typed `{` corrupts the msgID parse on the receiver.
**Fix:** strip or reject `{`, `|`, `~` from message bodies before encode.

### 27. `serialConn.Close()` doc/code mismatch ✅ DONE
**File:** `internal/rf/serialconn.go:99-103`
Comment says "Does NOT close the underlying file — caller's responsibility" but the body calls `c.f.Close()`. Risk of double-close if caller follows the docstring.
**Fix:** pick one (recommend keeping the close; fix the comment).

### 28. Trail teleport check has no `cos(lat)` correction ✅ DONE
**File:** `internal/server/routes.go:488`
`distKm := (dLat + dLon) * degToKm` over-weights longitude. Near the poles, longitude error is amplified; near the equator, the absolute-sum (vs proper haversine) overestimates by up to √2.
**Fix:** `dLonKm := dLon * degToKm * cos(lat)`; sum or use haversine.

### 29. `tnc.ChooseFreeRFCOMM` silent fallback to `/dev/rfcomm0` ✅ DONE
**File:** `internal/tnc/discover.go:321-329`
When all 32 slots are full, returns `/dev/rfcomm0` (which is bound). Overwrites whatever's there.
**Fix:** return `""`/error; surface to UI.

---

## LOW (12+)

- **30.** No 512-byte cap on outbound TNC2 — `internal/igate/igate.go:319`. Servers truncate/reject. ✅ DONE
- **31.** No TCP keepalive on APRS-IS connection — `internal/igate/igate.go:167`. NAT timeouts undetected until 120s read deadline. ✅ DONE (read deadline 120s→60s; APRS-IS sends `#` keepalives ~every 20s)
- **32.** AX.25 dest SSID byte C-bit not asserted — `internal/ax25/ax25.go:60-83`. UI command frame reads as AX.25 v1 to strict decoders. ❌ WON'T FIX (spec explicitly allows both-C-zero as pre-v2.0; aprx reference implementation does the same)
- **33.** KISS decoder passes trailing lone FESC through as `0xDB` — `internal/ax25/kiss.go:40-55`. Spec asks for frame rejection. ✅ DONE (spec actually says "ignore and continue", not reject — fixed: drop both bytes on invalid escape, drop trailing lone FESC)
- **34.** Status `>` doesn't strip optional 7-char timestamp — `internal/aprs/info.go:133`. ✅ DONE (per APRS101 §16, status is `>Comments` or `>DDHHMMzComments`; now strips the 7-byte zulu prefix when present)
- **35.** Course=000 treated as 0°/north rather than "unknown" (APRS101 convention) — `internal/aprs/info.go:170`, Mic-E branch ~624. ✅ DONE (uncompressed CSE/SPD and Mic-E both now require course in 1-360; 360=north, 0=unknown per Ham::APRS::FAP. Compressed format unaffected — already uses cs[0]==' ' as no-data signal)
- **36.** Position ambiguity level not surfaced on `Decoded` — `internal/aprs/info.go:363-387`. ✅ DONE (added `Decoded.Ambiguity int`; populated 0-4 from lat-side space count in uncompressed parser)
- **37.** Weather snow `s` regex matches but no switch case — `internal/aprs/weather.go:57`. Snow silently dropped. ✅ DONE (added `case "s"` per `sNNN` spec — 3-digit snowfall, hundredths inch 24hr; added `Weather.SnowHundIn`/`SnowSet`)
- **38.** No AX.25 §3.12.2 `*` (H-bit) contiguity validation in path parser — `internal/aprs/path.go`. Malformed paths with `*` non-contiguous accepted. ❌ WON'T FIX (gate logic already correctly handles via "first unused" — UI cosmetic only)
- **39.** Symbol field validated by length only, not char-class — `internal/state/state.go:380-382`. ❌ WON'T FIX (already validates 0x21-0x7E at sanitize.go:167 — audit was outdated)
- **40.** PARM./UNIT./EQNS./BITS. telemetry messages parsed as plain msgs — `internal/aprs/info.go:702`. ❌ WON'T FIX (feature, not bug)
- **41.** `!DAO!` datum / micro-precision extension not parsed (aprs12/datum.txt). ❌ WON'T FIX (feature, not bug)
- **42.** `internal/aprs/phg.go:41` regex doesn't match `PHGxxxxR` range-override form. ✅ DONE (regex now accepts optional 5th byte `[\dR]?` — covers both PHGR rate-suffix dialects seen in the wild)
- **43.** `internal/server/sanitize.go validateBeaconText` DTI whitelist too restrictive (rejects valid `;` `)` `_` `'` `` ` `` `>` `T`). ❌ WON'T FIX (function is dead code — never called; beacons are synthesized, no raw info-field input)
- **44.** `internal/store/store.go MarkAck` case-sensitive on source/dest/msgID. ✅ DONE (UPPER() wrap on both MarkAck and MarkRej — callsigns/msgIDs per APRS convention are case-insensitive)
- **45.** `internal/store/store.go Conversations` CTE `last_acked` semantics misleading for incoming-last threads. ❌ WON'T FIX (UI semantics, not protocol bug)

---

## Withdrawn during verification (false positives from early passes)

- **WIDEn-N decrement: H-bit not set on prior hops** — `Path[:idx]` are by construction already-used (idx is the first `!used` index), so H-bits are preserved correctly.
- **Mic-E speed wrap on `speedKt >= 800`** — algebraically equivalent to `sp >= 80` wrap before multiply: `(sp*10 + dc/10) − 800 == (sp−80)*10 + dc/10`.
- **Loop-prevention SSID-sensitive compare** — different SSIDs are correctly distinct stations per AX.25 / APRS convention. `N0CALL-7` ≠ `N0CALL-10`.
- **Message retry cadence (fixed 30s × 5)** — APRS101 §14 has no normative cadence. Xastir, UI-View, APRSdroid all use fixed ~30s. Only APRSISCE/32 uses Bruninga's decay. aprgo matches the majority.
- **Mic-E `Z` ambiguity translation** — per mic-e-types.txt, Z encodes lat=space + N-lat + 100°-lon offset + Custom-A msg bit; aprgo correctly translates Z to ambiguity placeholder and derives hemisphere/offset from the raw `dst[]` bytes (which retain Z) before the digit/space overwrite.

---

## Recommended implementation order

1. **HIGH:** block `}` in `decideFromIS` (#1); Object offset 11→18 (#2); add `IsRej` (#3); split reply-ack on `}` (#4).
2. **High-impact MED:** reconnect backoff floor 30s (#5); HeardOnRF case-fold (#6); `DefaultIGateRecentRFMinutes` 360→30 (#7); `qAR`/`qAO` per capability (#10).
3. **Parser MED:** compressed cs/T altitude (#15); Mic-E `xxx}` altitude (#16); positionless WX (#17); Mic-E lonMin reject 60-69 (#18); 67-char cap (#20); base-91 telemetry (#21).
4. **Protocol robustness:** AX.25 charset validation (#11); beacon comment sanitize + cap (#12); KISS TXDELAY/PERSIST on connect (#13); beacon-path regex (#23); wizard validation (#24); msgID collision (#25); body `{`/`|`/`~` filter (#26).
5. **Behavior gap:** messaged-to position gating (#9); `?IGATE?` reply (#14); IS→RF configurable path (#22).
6. **Robustness/UX:** serialConn close (#27); teleport `cos(lat)` (#28); ChooseFreeRFCOMM (#29).
7. **LOW:** sweep through items 30-45 as opportunity allows.

Spec source citations available in audit transcript; key references:
- http://www.aprs.org/doc/APRS101.PDF
- http://www.aprs.org/fix14439.html
- http://www.aprs.org/aprs12/mic-e-types.txt
- http://www.aprs.org/aprs11/spec-wx.txt
- http://www.aprs.org/aprs11/replyacks.txt
- http://www.aprs-is.net/IGating.aspx
- http://www.aprs-is.net/q.aspx
- http://he.fi/doc/aprs-base91-comment-telemetry.txt
- AX.25 Link Access Protocol v2.2

---

## Future research (not yet audited)

These came up in conversation and warrant a deeper look later. Each needs spec study before scoping a change.

### Group messaging — RESEARCHED, NOT YET BUILT

**Spec recap** (APRS101 §14 + PROTOCOL.TXT):
- `:BLN0     :` through `:BLN9     :` — **numbered bulletins**, always-displayed broadcasts. Latest with same digit replaces prior.
- `:BLNA     :` through `:BLNZ     :` — **announcements** (sticky display, longer decay; same identifier replaces).
- `:BLN1ARES :`, `:BLN1WX   :` — **group bulletins**, 1-5 char group name. No registry — clubs/regions agree informally.
- `:NWS-xxxx :`, `:SKYxxxxx :` — bulletin-form, severity encoded in source-callsign product code (`FWDFFW` = Fort Worth flash-flood). Already handled by source-prefix bypass (#58).
- `:ALL      :` — **not in spec**; convention only. Display locally, don't IS↔RF gate.

**Bruninga group whitelist rule:** operator maintains a personal group list. Empty = receive everything. Non-empty = receive plain `BLN0-9` always, plus only the listed groups. Real-world implementation in Xastir, YAAC, APRSIS32.

**How real radios do it:**
- **Kenwood** (TH-D74/D75, TM-D710G): all messages in one unified list, type-tagged (M/G/B). Pre-programmed group-code list (default `ALL CQ QST` + slot for BLN/NWS). Anything not matching dropped at reception.
- **Yaesu** (FTM-400/500/300, FT2D/3D/5D): mixed list with M/G/B prefix. 6 MESSAGE GROUP slots (default `ALL CQ QST YAESU` + 2 free) + 6 BULLETIN GROUP slots. White-LED strobe + ringer on receipt.
- **Anytone, cheap Chinese**: TX-only / no message UI; bulletins effectively invisible.
- **What no radio does**: NWS severity tones, tactical address book (`N0CALL-10` → friendly name), separate bulletin marquee.

**Suggested aprgo implementation (3 pieces):**
1. **Group whitelist setting** in state: `MessageGroups []string` defaulting to `["ALL", "CQ", "QST"]`. Optional `BulletinGroups []string` (default empty = all bulletins accepted). UI: two text inputs in Settings → Identity area.
2. **Filter rule** on inbound messages: if MsgTo is a bulletin/group form, require it to match the operator's lists; otherwise drop from display (still log for audit).
3. **UI tab/section** for bulletins separate from 1:1 chat, keyed on identifier (latest-replaces semantics). We already parse `BulletinGroup` from #68.

Differentiator opportunities (since no hardware does these):
- NWS severity color-coding by product-code prefix
- Tactical address book mapping callsign-SSID → friendly name
- Per-message sound/notification by group

### Extra-long messages — SKIPPED
APRS101 caps a single message body at 67 chars. There is **no formal spec extension** for multi-part — Bruninga (messages.txt) explicitly opposes it: "APRS messages are one-liner's. Users must understand this." None of Xastir, APRSdroid, YAAC, APRSIS32, APRS Messenger, or Kenwood D74/D75 implements segmentation.

**One outlier**: Graywolf (chrissnell/graywolf — a Go APRS station, same category as aprgo) splits long bodies into multiple spec-compliant 67-byte fragments. Marker `{1/2}` / `{2/2}` rides in the msgID slot (after `{`), not in the body, so each fragment is a valid standalone APRS message. Graywolf↔Graywolf auto-reassembles; non-Graywolf clients render N separate messages and ack each individually.

**Decision: SKIP.** Implementing Graywolf's scheme would be a Graywolf-only interop feature; our 67-char clamp matches every other client. Receive-side parsing of `{N/M}` msgIDs (~20 LOC, no TX changes) could be added later if a Graywolf community appears in the user base.

Source: https://chrissnell.com/software/graywolf/messaging.html

### Other candidates seen in passing
- Telemetry compression: PARM./UNIT./EQNS./BITS. config payloads (mentioned in #40 but never decoded as structured telemetry config in our store).
- `!DAO!` micro-precision datum extension (#41) — small but real position accuracy gain.
- APRS 1.2 / NMEA-style hot patches (DTI `$`) — would let us decode some legacy/cheap trackers we currently ignore.

---

# Second-pass audit (post-45-item)

Four parallel subagents (HTTP security, concurrency, protocol gaps not covered, recently-added code) found these. Numbering starts at 46.

## HIGH

### 46. MSG_CNT underflow in `?IGATE?` reply ✅ DONE
**File:** `internal/server/queries.go:63`
`s.stats.sentRF.Load() - s.stats.digipeats.Load()` is a non-atomic compound. If a digipeat lands between the two atomic reads, `uint64` subtraction wraps to ~18 quintillion.
**Fix:** load both into locals; clamp `if a < b { msgCnt = 0 } else { msgCnt = a - b }`.

### 47. `queries.go time.AfterFunc` leaks past shutdown ✅ DONE
**File:** `internal/server/queries.go:55`
Scheduled 0-30s query responses don't observe ctx cancel. May TX after shutdown begins.
**Fix:** track timers in `Server` and `Stop()` them in shutdown path.

### 48. `viscous` AfterFunc + state-subscriber goroutines + btSupervisor not joined on shutdown ✅ DONE
**File:** `internal/server/viscous.go:60-70`, `internal/igate/igate.go:111`, `internal/rf/rf.go:111,116`
Multiple post-shutdown goroutine leaks. Adds up to ~5s of post-shutdown TX risk.
**Fix:** add `stop()` for viscous queue; have rf.Run wait on btSupervisor; thread WaitGroups through subscriber goroutines.

### 49. `NextOutboundMsgID` disk thrash ✅ DONE + silent save error
**File:** `internal/state/state.go:335-348`
Saves to disk on every outbound message under the state mutex. SD-card wear; blocks every Snapshot RLock. `_ = s.save()` swallows disk-full → counter advances in memory but not disk → after restart, IDs collide.
**Fix:** reserve a block (e.g., 100 IDs) on disk, only re-save when block exhausted; surface save errors.

### 50. `serialconn.go` context-watcher goroutine is dead code ✅ DONE
**File:** `internal/rf/serialconn.go:46-53`
Goroutine waits for `ctx.Done()` or `c.closed`, then does NOTHING. Either delete or `unix.Shutdown(fd)` to actually interrupt the blocking poll.

### 51. `validAX25BaseCall` rejects lowercase decoded callsigns ✅ DONE
**File:** `internal/ax25/ax25.go:183`
`decodeCallsign` doesn't uppercase the bit-shifted bytes. If a non-conforming TNC emits lowercase, the entire frame is rejected.
**Fix:** uppercase in validator, or document the invariant + uppercase at decode time.

### 52. `pairStore.gc()` defined but never called ✅ DONE
**File:** `internal/server/pairing.go:58`
Pair jobs accumulate forever — each retains a 45s timer context + cancel closure.
**Fix:** janitor goroutine like `wdraftsJanitor` calling `s.pairs.gc()` every 5 min.

## MED

### 53. `store.Store.mu` serializes reads against writes ⏸ DEFERRED (research: redundant with WAL+busy_timeout but removal needs callsite audit; current perf adequate, defer to perf pass)
**File:** `internal/store/store.go:21`
Re-serializes everything despite SQLite WAL. Long queries block UpsertHeard, manifests as RF receive stalls during dashboard polls.
**Fix:** drop the mutex or scope to multi-statement TXs only.

### 54. `ratelimit.clientIP` ignores ✅ DONE `X-Forwarded-For`
**File:** `internal/server/ratelimit.go:77-83`
Docstring promises XFF on loopback; implementation doesn't honor it. Behind a reverse proxy, all logins coalesce to one IP.

### 55. Trail filter drops equator/prime-meridian positions ✅ DONE
**File:** `internal/server/routes.go:484`
`p.Lat == 0 || p.Lon == 0` — should be `&&` (both-zero is the cold-boot pattern).

### 56. Mic-E `}` strip eats legitimate trailing text ✅ DONE
**File:** `internal/aprs/info.go:777`
When `}` appears in comment text with no 3-byte base-91 altitude prefix, code still slices `c = c[i+1:]`.
**Fix:** only consume `}` when altitude actually decoded.

### 57. Mic-E `posambig` not surfaced ✅ DONE on `Decoded.Ambiguity`
**File:** `internal/aprs/info.go:658-673`
Mic-E only uses it internally. Add `d.Ambiguity = posambig`.

### 58. NWS / SKYWARN / CWA bulletins dropped from IS→RF ✅ DONE
**File:** `internal/gate/gate.go:421`
`heardOnRF(MsgTo)` rejects all NWS bulletins because nobody locally has the NWS callsign. Bruninga WXSVR design wants these relayed.
**Fix:** whitelist `^(NWS|SKY|CWA)` source prefixes to bypass heardOnRF.

### 59. JSON endpoints unrate-limited ⏸ DEFERRED (would add golang.org/x/time/rate dependency for low-risk authed-user spam; defer to perf pass)
`/api/stations`, `/api/trails`, `/api/feed` open to authed-user spam. Add per-session token bucket.

### 60. Mic-E status bytes 0x1C-0x1F discarded ✅ DONE
**File:** `internal/aprs/info.go:789-793`
Capture into `Decoded.MicEFixStatus` before the whitespace trim.

### 61. SSn-N regional flood aliases not honored ✅ DONE
**File:** `internal/gate/gate.go parseWIDE`
`ARIZ1-1`, `MASS2-2`, `NCN1-1` silently ignored — only literal `WIDE` matched.

### 62. Raw NMEA ✅ DONE `$GPRMC`/`$GPGGA` packets dropped
**File:** `internal/aprs/info.go:122` (DTI switch)
No `case '$'`. Legacy/cheap trackers lose position.

### 63. Passcode verification inconsistent between wizard and settings save ❌ INVALID (verified: both paths call aprsISPasscodeMatches; settings at routes.go:1121)
**File:** `internal/server/sanitize.go:208 validatePasscode` vs `wizard.go:251`
Wizard verifies match; settings save only checks numeric.
**Fix:** have both call `aprsISPasscodeMatches`.

### 64. APRS-IS filter string not validated ⏸ DEFERRED (full grammar lint is ~150 LOC; failure mode is operator-visible "filter does nothing"; defer until users hit it)
Passed verbatim to login. Malformed filter silently drops all traffic. Add `ValidateISFilter()`.

### 65. Heard-direct-vs-digipeated not tracked ❌ INVALID (research: Bruninga convention is heard-locally direct-OR-digi within window; current behavior matches)
IS→RF relay to any RF-heard station, even 3 digis away. Add `HeardDirect` (filtered to `DigiCount==0`).

## LOW

### 66. Item parser doesn't enforce name length 3-9 ✅ DONE
`internal/aprs/info.go:141`

### 67. Frequency parser ignores CTCSS ✅ DONE tone / offset
`internal/aprs/info.go freqRegex`

### 68. Bulletin group form ✅ DONE `:BLNxAAAAA:` not surfaced
`internal/aprs/info.go decodeMessage` — add `Decoded.BulletinGroup`.

### 69. `recent[]` ring buffer slice-shift is O(n) per packet ✅ DONE
`internal/server/server.go:436-439` — use head/tail indices.

### 70. dupeTable GC walks entire map under contended lock ✅ DONE
`internal/server/dupe.go:34-40`

### 71. sourceRateLimiter.maybeCleanup only fires on first-seen insert ✅ DONE
`internal/server/ratelimit_source.go:55-57` — map can grow unbounded.

### 72. msgTracker no upper bound on map size ✅ DONE
`internal/gate/tracker.go` — gc only deletes expired; add LRU cap.

### 73. retryQueue.due() iteration order is random ✅ DONE
`internal/server/retry.go:80-90` — sort by NextRetry.

### 74. KISS buf retains backing array forever ✅ DONE
`internal/ax25/kiss.go:127` — periodic `copy` to fresh slice.

### 75. Pre-1.0 `/1`-`/9` legacy GPS DTIs not parsed ✅ DONE
`internal/aprs/info.go:122` — real-world traffic ~zero; document non-support.

### 76. `originHostMatches` doesn't strip port from Host ❌ INVALID (verified: both sides include port symmetrically)
`internal/server/sanitize.go:296-306` — cosmetic.

### 77. Cookie missing `Secure` auto-detection ✅ DONE
`internal/auth/auth.go:58-68` — auto-enable when `r.TLS != nil`.

### 78. Bare `\n`-terminated outbound becomes ✅ DONE `\n\r\n`
`internal/igate/igate.go:320` — cosmetic.

### 79. Non-Advanced operators can still POST tnc_*, theme, retention_days ❌ INVALID (verified: TNC inputs render in all modes via ### 79. Non-Advanced operators can still POST tnc_*, theme, retention_dayslt;details### 79. Non-Advanced operators can still POST tnc_*, theme, retention_daysgt;; theme/retention/timezone are intentional cross-mode user prefs)
`internal/server/routes.go:1182-1205` — clamped to safe ranges; mass-assignment surface only.

## Test coverage gaps

### 80. Zero tests for new code ✅ DONE
Only `passcode_test.go` exists. Missing: `tracker.go`, `serialconn.go`, `queries.go`, KISS param encode + new escape handling, compressed-position cs/T altitude, Mic-E `xxx}` altitude, base-91 telemetry, positionless WX, `validAX25BaseCall`, `NextOutboundMsgID` wrap, trail teleport. High regression risk.
