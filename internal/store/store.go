// Package store wraps SQLite for stations, messages, and recent packets.
package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"aprgo/internal/ax25"
)

// Store is the persistent store handle.
type Store struct {
	db *sql.DB
	mu sync.RWMutex
}

const schema = `
CREATE TABLE IF NOT EXISTS stations (
    callsign     TEXT PRIMARY KEY,
    last_seen    INTEGER NOT NULL,
    last_path    TEXT,
    last_info    TEXT,
    last_dest    TEXT,
    last_source  TEXT,         -- 'RF' | 'IS'
    last_seen_rf INTEGER,      -- last_seen restricted to RF-source receptions
    lat          REAL,
    lon          REAL,
    symbol       TEXT,
    comment      TEXT,
    pkt_count    INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_stations_last_seen ON stations(last_seen DESC);
CREATE INDEX IF NOT EXISTS idx_stations_last_seen_rf ON stations(last_seen_rf DESC);

CREATE TABLE IF NOT EXISTS messages (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    ts           INTEGER NOT NULL,
    direction    TEXT NOT NULL CHECK (direction IN ('out','in')),
    source       TEXT NOT NULL,
    dest         TEXT NOT NULL,
    body         TEXT NOT NULL,
    msg_id       TEXT,
    via_rf       INTEGER NOT NULL DEFAULT 0,
    via_is       INTEGER NOT NULL DEFAULT 0,
    acked        INTEGER NOT NULL DEFAULT 0,
    raw          TEXT,
    state        TEXT NOT NULL DEFAULT 'acked',     -- pending|acked|failed|cancelled
    attempts     INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_messages_ts ON messages(ts DESC);
CREATE INDEX IF NOT EXISTS idx_messages_thread ON messages(source, dest);

CREATE TABLE IF NOT EXISTS packets (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    ts           INTEGER NOT NULL,
    source       TEXT NOT NULL,
    dest         TEXT,
    path         TEXT,
    info         TEXT,
    src_kind     TEXT,          -- 'RF' | 'IS'
    raw          TEXT,
    -- Parsed position cached so the /api/trails read path can SELECT
    -- directly without re-parsing every info field. NULL for non-position
    -- packets (message frames, ACKs, status, telemetry, etc).
    lat          REAL,
    lon          REAL
);
CREATE INDEX IF NOT EXISTS idx_packets_source_ts ON packets(source, ts DESC);
CREATE INDEX IF NOT EXISTS idx_packets_ts ON packets(ts DESC);
CREATE INDEX IF NOT EXISTS idx_packets_pos_ts ON packets(ts) WHERE lat IS NOT NULL;
`

// Open opens the SQLite store at path, creating the schema if missing.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("mkdir: %w", err)
	}
	// modernc.org/sqlite uses pragmas via _pragma=... params (not _journal_mode).
	// Tuned for SD-card-backed deploys (Pi Zero, embedded). Net effect:
	//   - WAL + NORMAL: append-mostly writes, no per-commit fsync
	//   - 16 MB page cache: repeated reads never touch the card
	//   - temp_store=MEMORY: sort/group-by stays in RAM, not on SD
	//   - 64 MB mmap: read-heavy endpoints (/api/trails, conversations)
	//     get OS-page-cached random access
	dsn := path + "?_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=cache_size(-16000)" +
		"&_pragma=temp_store(MEMORY)" +
		"&_pragma=mmap_size(67108864)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Station is a row from the stations table.
type Station struct {
	Callsign   string
	LastSeen   time.Time
	LastPath   string
	LastInfo   string
	LastDest   string
	LastSource string // "RF" | "IS"
	LastSeenRF sql.NullInt64
	Lat, Lon   sql.NullFloat64
	Symbol     string
	Comment    string
	PktCount   int
}

// UpsertHeard records or updates a station from an RX event.
func (s *Store) UpsertHeard(callsign string, t time.Time, path, info, dest string, src ax25.Source, lat, lon *float64, symbol, comment string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	srcStr := src.String()
	var seenRF any
	if src == ax25.SrcRF {
		seenRF = t.Unix()
	} else {
		seenRF = nil
	}
	if _, err := tx.Exec(`INSERT INTO stations (callsign, last_seen, last_path, last_info, last_dest, last_source, last_seen_rf, pkt_count)
        VALUES (?, ?, ?, ?, ?, ?, ?, 1)
        ON CONFLICT(callsign) DO UPDATE SET
            last_seen = excluded.last_seen,
            last_path = excluded.last_path,
            last_info = excluded.last_info,
            last_dest = excluded.last_dest,
            last_source = excluded.last_source,
            last_seen_rf = COALESCE(excluded.last_seen_rf, stations.last_seen_rf),
            pkt_count = pkt_count + 1`,
		callsign, t.Unix(), path, info, dest, srcStr, seenRF); err != nil {
		return err
	}
	if lat != nil && lon != nil {
		if _, err := tx.Exec(`UPDATE stations SET lat=?, lon=?, symbol=COALESCE(NULLIF(?, ''), symbol), comment=COALESCE(NULLIF(?, ''), comment) WHERE callsign=?`,
			*lat, *lon, symbol, comment, callsign); err != nil {
			return err
		}
	} else if symbol != "" || comment != "" {
		if _, err := tx.Exec(`UPDATE stations SET symbol=COALESCE(NULLIF(?, ''), symbol), comment=COALESCE(NULLIF(?, ''), comment) WHERE callsign=?`,
			symbol, comment, callsign); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// HeardSince returns stations seen since cutoff, newest first.
func (s *Store) HeardSince(cutoff time.Time) ([]Station, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.Query(`SELECT callsign, last_seen, COALESCE(last_path, ''), COALESCE(last_info, ''),
        COALESCE(last_dest, ''), COALESCE(last_source, ''), last_seen_rf,
        lat, lon, COALESCE(symbol, ''), COALESCE(comment, ''), pkt_count
        FROM stations WHERE last_seen >= ? ORDER BY last_seen DESC`, cutoff.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Station
	for rows.Next() {
		var st Station
		var ts int64
		if err := rows.Scan(&st.Callsign, &ts, &st.LastPath, &st.LastInfo, &st.LastDest, &st.LastSource, &st.LastSeenRF,
			&st.Lat, &st.Lon, &st.Symbol, &st.Comment, &st.PktCount); err != nil {
			return nil, err
		}
		st.LastSeen = time.Unix(ts, 0)
		out = append(out, st)
	}
	return out, rows.Err()
}

// HeardOnRF reports whether callsign has been heard via RF since `since`.
// Used by Phase 3 IS→RF gating.
func (s *Store) HeardOnRF(callsign string, since time.Time) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var ts sql.NullInt64
	err := s.db.QueryRow(`SELECT last_seen_rf FROM stations WHERE callsign = ?`, callsign).Scan(&ts)
	if err != nil || !ts.Valid {
		return false
	}
	return ts.Int64 >= since.Unix()
}

// Message is a row from messages.
type Message struct {
	ID        int64
	Time      time.Time
	Direction string // "out" | "in"
	Source    string
	Dest      string
	Body      string
	MsgID     string
	ViaRF     bool
	ViaIS     bool
	Acked     bool
	Raw       string
	// State is the lifecycle state of an outgoing message:
	//   pending   — queued / in retry window, awaiting ack
	//   acked     — recipient sent back an ack (or the message is incoming)
	//   failed    — all retries exhausted, no ack received
	//   cancelled — user pressed × while retries were in flight
	// Incoming messages are always stored as state='acked' for consistency.
	State    string
	Attempts int
}

// LogMessage stores a sent or received APRS message. Callers handling
// outbound messages with retries should set State to "pending"; incoming
// messages should leave it empty (defaults to "acked", since there's no
// retry lifecycle on RX).
func (s *Store) LogMessage(m Message) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := m.State
	if state == "" {
		state = "acked"
	}
	r, err := s.db.Exec(`INSERT INTO messages (ts, direction, source, dest, body, msg_id, via_rf, via_is, acked, raw, state, attempts)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.Time.Unix(), m.Direction, m.Source, m.Dest, m.Body, m.MsgID, b2i(m.ViaRF), b2i(m.ViaIS), b2i(m.Acked), m.Raw, state, m.Attempts)
	if err != nil {
		return 0, err
	}
	return r.LastInsertId()
}

// SetMessageState updates the retry-state lifecycle of an outgoing message.
// `attempts` is the current count of send attempts (0 = original send only).
// Sets the `acked` boolean column in sync with state for the existing UI
// code that filters on it.
func (s *Store) SetMessageState(id int64, state string, attempts int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	acked := 0
	if state == "acked" {
		acked = 1
	}
	_, err := s.db.Exec(`UPDATE messages SET state=?, attempts=?, acked=? WHERE id=?`,
		state, attempts, acked, id)
	return err
}

// GetMessage returns one message by ID. Used by retry/cancel handlers to
// look up the original send so we can re-encode and re-fire it.
func (s *Store) GetMessage(id int64) (Message, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var m Message
	var ts int64
	var rf, is, acked int
	err := s.db.QueryRow(`SELECT id, ts, direction, source, dest, body, COALESCE(msg_id,''), via_rf, via_is, acked, COALESCE(raw,''), state, attempts
		FROM messages WHERE id=?`, id).Scan(&m.ID, &ts, &m.Direction, &m.Source, &m.Dest, &m.Body, &m.MsgID, &rf, &is, &acked, &m.Raw, &m.State, &m.Attempts)
	if err != nil {
		return Message{}, err
	}
	m.Time = time.Unix(ts, 0)
	m.ViaRF = rf != 0
	m.ViaIS = is != 0
	m.Acked = acked != 0
	return m, nil
}

// MarkAck marks the most recent outgoing message acked. Also collapses
// retry state to "acked" so the retry worker drops it from its in-memory
// queue on the next sweep.
func (s *Store) MarkAck(source, dest, msgID string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Find the most recent matching pending/unacked outbound row so we can
	// return its ID (the retry worker uses this to dequeue immediately).
	var id int64
	err := s.db.QueryRow(`SELECT id FROM messages
		WHERE direction='out' AND source=? AND dest=? AND msg_id=? AND state IN ('pending','acked') AND acked=0
		ORDER BY id DESC LIMIT 1`,
		source, dest, msgID).Scan(&id)
	if err != nil {
		// No matching row — possibly an ack for a message we don't have
		// in our log (e.g. iGate started after a peer's message). That's
		// fine; nothing to do.
		if err == sql.ErrNoRows {
			return 0, nil
		}
		return 0, err
	}
	if _, err := s.db.Exec(`UPDATE messages SET acked=1, state='acked' WHERE id=?`, id); err != nil {
		return 0, err
	}
	return id, nil
}

// Prune deletes rows older than cutoff in stations, messages, packets in a
// single transaction so a crash mid-prune leaves a consistent DB state.
func (s *Store) Prune(cutoff time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := cutoff.Unix()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	r1, err := tx.Exec(`DELETE FROM stations WHERE last_seen < ?`, c)
	if err != nil {
		return 0, err
	}
	r2, err := tx.Exec(`DELETE FROM messages WHERE ts < ?`, c)
	if err != nil {
		return 0, err
	}
	r3, err := tx.Exec(`DELETE FROM packets WHERE ts < ?`, c)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	a, _ := r1.RowsAffected()
	b, _ := r2.RowsAffected()
	d, _ := r3.RowsAffected()
	return a + b + d, nil
}

// LogPacket stores a single packet for per-station history views.
// LogPacket inserts a packet row. lat/lon may be nil (unparsed position or
// no position in the frame); the columns store NULL in that case.
func (s *Store) LogPacket(ts time.Time, source, dest, path, info string, src ax25.Source, raw string, lat, lon *float64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var latArg, lonArg any
	if lat != nil {
		latArg = *lat
	}
	if lon != nil {
		lonArg = *lon
	}
	_, err := s.db.Exec(`INSERT INTO packets (ts, source, dest, path, info, src_kind, raw, lat, lon)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ts.Unix(), source, dest, path, info, src.String(), raw, latArg, lonArg)
	return err
}

// PacketPosition is the lean per-row shape used to build trails — just the
// callsign, position, and time. Avoids materializing the full info text
// for every packet in a 3-day window.
type PacketPosition struct {
	Source string
	Lat    float64
	Lon    float64
	Time   time.Time
}

// PacketPositionsSince returns every (lat, lon, ts) tuple newer than `since`,
// oldest first. Trails endpoint walks these to build per-source polylines
// without ever touching the `info` field. Indexed via idx_packets_pos_ts so
// non-position packets (NULL lat) are skipped at the index level.
func (s *Store) PacketPositionsSince(since time.Time) ([]PacketPosition, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.Query(`SELECT source, lat, lon, ts FROM packets
		WHERE ts >= ? AND lat IS NOT NULL AND lon IS NOT NULL
		ORDER BY ts ASC`, since.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PacketPosition
	for rows.Next() {
		var p PacketPosition
		var ts int64
		if err := rows.Scan(&p.Source, &p.Lat, &p.Lon, &ts); err != nil {
			return nil, err
		}
		p.Time = time.Unix(ts, 0)
		out = append(out, p)
	}
	return out, rows.Err()
}

// LoggedPacket is one row from the packets table.
type LoggedPacket struct {
	ID      int64
	Time    time.Time
	Source  string
	Dest    string
	Path    string
	Info    string
	SrcKind string
	Raw     string
}

// PacketsBySource returns the most recent N packets from a given source.
func (s *Store) PacketsBySource(source string, limit int) ([]LoggedPacket, error) {
	if limit <= 0 || limit > 2000 {
		limit = 2000
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.Query(`SELECT id, ts, source, COALESCE(dest, ''), COALESCE(path, ''), COALESCE(info, ''),
        COALESCE(src_kind, ''), COALESCE(raw, '')
        FROM packets WHERE source = ? ORDER BY ts DESC LIMIT ?`, source, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LoggedPacket
	for rows.Next() {
		var p LoggedPacket
		var ts int64
		if err := rows.Scan(&p.ID, &ts, &p.Source, &p.Dest, &p.Path, &p.Info, &p.SrcKind, &p.Raw); err != nil {
			return nil, err
		}
		p.Time = time.Unix(ts, 0)
		out = append(out, p)
	}
	return out, rows.Err()
}

// Conversation is the chat-list summary of all messages with a given peer.
// Peer is the *other* party: the dest for outgoing messages, the source for
// incoming ones. Ordered most-recent-activity first.
type Conversation struct {
	Peer        string    // other party's callsign
	LastTime    time.Time // newest message time
	LastBody    string    // newest body (truncated downstream if needed)
	LastDir     string    // "in" | "out"
	LastAcked   bool      // outgoing-only; meaningless for incoming
	Count       int       // total messages with this peer
}

// Conversations returns one row per unique peer (collapsing both directions),
// sorted newest-first. `me` is the operator's callsign — exchanges that don't
// touch us are excluded so beacon-passthrough chatter doesn't pollute the list.
func (s *Store) Conversations(me string) ([]Conversation, error) {
	if me == "" {
		return nil, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.Query(`
		WITH paired AS (
			SELECT
				CASE WHEN direction='out' THEN dest ELSE source END AS peer,
				ts, direction, body, acked
			FROM messages
			WHERE (direction='out' AND source=?) OR (direction='in' AND dest=?)
		)
		SELECT peer,
		       MAX(ts) AS last_ts,
		       (SELECT body      FROM paired p2 WHERE p2.peer = paired.peer ORDER BY ts DESC LIMIT 1) AS last_body,
		       (SELECT direction FROM paired p3 WHERE p3.peer = paired.peer ORDER BY ts DESC LIMIT 1) AS last_dir,
		       (SELECT acked     FROM paired p4 WHERE p4.peer = paired.peer ORDER BY ts DESC LIMIT 1) AS last_acked,
		       COUNT(*) AS n
		FROM paired
		GROUP BY peer
		ORDER BY last_ts DESC`, me, me)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Conversation
	for rows.Next() {
		var c Conversation
		var ts int64
		var acked int
		if err := rows.Scan(&c.Peer, &ts, &c.LastBody, &c.LastDir, &acked, &c.Count); err != nil {
			return nil, err
		}
		c.LastTime = time.Unix(ts, 0)
		c.LastAcked = acked != 0
		out = append(out, c)
	}
	return out, rows.Err()
}

// MessagesWithPeer returns the message thread between `me` and `peer`, oldest
// first (chat-style: scroll-to-bottom rendering). Caller can flip downstream.
func (s *Store) MessagesWithPeer(me, peer string, limit int) ([]Message, error) {
	if limit <= 0 || limit > 1000 {
		limit = 500
	}
	if me == "" || peer == "" {
		return nil, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	// Pull newest N, return ascending so the template renders oldest-on-top.
	rows, err := s.db.Query(`SELECT id, ts, direction, source, dest, body, COALESCE(msg_id,''), via_rf, via_is, acked, COALESCE(raw,''), state, attempts
		FROM messages
		WHERE (direction='out' AND source=? AND dest=?)
		   OR (direction='in'  AND source=? AND dest=?)
		ORDER BY id DESC LIMIT ?`, me, peer, peer, me, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var m Message
		var ts int64
		var rf, is, acked int
		if err := rows.Scan(&m.ID, &ts, &m.Direction, &m.Source, &m.Dest, &m.Body, &m.MsgID, &rf, &is, &acked, &m.Raw, &m.State, &m.Attempts); err != nil {
			return nil, err
		}
		m.Time = time.Unix(ts, 0)
		m.ViaRF = rf != 0
		m.ViaIS = is != 0
		m.Acked = acked != 0
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Reverse to ascending.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
