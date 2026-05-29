// Package webhook delivers received APRS packets to operator-configured
// outbound HTTP endpoints (Home Assistant, Node-RED, Discord, …).
//
// Design:
//   - Manager subscribes to bus.Packets — the clean, deduped, dashboard-
//     curated stream (NOT the raw IS firehose; see server.parseLoop).
//   - For each packet it evaluates every configured webhook's filters and,
//     for each match, enqueues a delivery job onto a bounded channel.
//   - A small worker pool drains the channel and POSTs each job with up to
//     maxAttempts tries and backoff. After the final failure the event is
//     dropped and the failure is logged (so it surfaces on the Logs page).
//
// The bounded channel + drop-on-full means a slow or unreachable endpoint
// can never back-pressure the bus or the RF reader: the bus already drops
// to slow subscribers, and we drain it promptly by handing slow HTTP work
// to the worker pool. This is the right tradeoff for home automation —
// fire-and-forget with brief retry, no durable queue, no SD-card churn.
package webhook

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"aprgo/internal/aprs"
	"aprgo/internal/bus"
	"aprgo/internal/state"
)

const (
	// maxAttempts is the total number of delivery tries before a job is
	// dropped and logged. The operator agreed: "after 3 failed retries, we
	// just drop it and log the failure."
	maxAttempts = 3
	// deliverTimeout bounds a single HTTP attempt (connect + request). Kept
	// short so one dead endpoint can't tie up a worker — critical in offline
	// deployments where a cloud target is simply unreachable.
	deliverTimeout = 10 * time.Second
	// queueDepth bounds in-flight jobs. APRS packet rates are low (a few/s
	// at most), so this absorbs bursts comfortably; if it ever fills, the
	// newest job is dropped and logged rather than blocking the bus reader.
	queueDepth = 256
	// numWorkers caps concurrent outbound POSTs. A handful is plenty and
	// keeps one slow endpoint from starving the others.
	numWorkers = 4
)

// EndpointStatus is the per-webhook health surfaced on the Settings page.
type EndpointStatus struct {
	LastAttempt time.Time // zero = never attempted
	LastCode    int       // last HTTP status; 0 = transport error / none
	LastErr     string    // last error text, "" = last attempt succeeded
	Sent        int64     // deliveries that returned 2xx
	Failed      int64     // jobs dropped after exhausting maxAttempts
}

type job struct {
	name      string // webhook Name (status key)
	url       string
	headerKey string
	headerVal string
	insecure  bool
	body      []byte
}

// Manager owns the dispatch loop, worker pool, HTTP clients, and per-endpoint
// status. One per process; safe for concurrent status reads.
type Manager struct {
	st  *state.Store
	bus *bus.Bus

	clientVerify   *http.Client
	clientInsecure *http.Client

	jobs chan job

	mu     sync.Mutex
	status map[string]*EndpointStatus
}

// New constructs a Manager. Run must be called to start it.
func New(st *state.Store, b *bus.Bus) *Manager {
	return &Manager{
		st:   st,
		bus:  b,
		jobs: make(chan job, queueDepth),
		clientVerify: &http.Client{
			Timeout: deliverTimeout,
		},
		clientInsecure: &http.Client{
			Timeout: deliverTimeout,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // operator-opt-in for self-signed LAN receivers
			},
		},
		status: make(map[string]*EndpointStatus),
	}
}

// Run subscribes to bus.Packets and drives delivery until ctx is cancelled.
// Started under the server's spawn() supervisor, so it blocks for the life of
// the process. Workers are owned by this call and drain before it returns, so
// a (panic-triggered) restart never leaks goroutines.
func (m *Manager) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.worker(ctx)
		}()
	}

	sub, cancel := m.bus.Packets.Subscribe()
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case pkt, ok := <-sub:
			if !ok {
				wg.Wait()
				return
			}
			m.dispatch(pkt)
		}
	}
}

// dispatch evaluates one packet against all configured webhooks and enqueues
// a delivery job per match. The body is marshalled once per matching webhook
// (cheap; differs only if we ever per-endpoint-customize it, which we don't
// today — but keeping it per-job keeps the job self-contained).
func (m *Manager) dispatch(pkt aprs.Packet) {
	whs := m.st.Snapshot().Webhooks
	if len(whs) == 0 {
		return
	}
	var body []byte
	for _, wh := range whs {
		if !wh.Enabled || wh.URL == "" {
			continue
		}
		if !Match(wh, pkt) {
			continue
		}
		if body == nil {
			b, err := buildBody(pkt, false)
			if err != nil {
				log.Printf("webhook: marshal packet failed: %v", err)
				return
			}
			body = b
		}
		j := job{
			name:      wh.Name,
			url:       wh.URL,
			headerKey: wh.HeaderName,
			headerVal: wh.HeaderValue,
			insecure:  wh.InsecureSkipTLS,
			body:      body,
		}
		select {
		case m.jobs <- j:
		default:
			// Queue full: a receiver is badly backed up. Drop the newest
			// event (bounded memory) and record it as a failure so the
			// operator sees it on the Settings status line + Logs page.
			m.recordFailure(wh.Name, "delivery queue full (receiver too slow)")
			log.Printf("webhook %q: queue full, dropped event", wh.Name)
		}
	}
}

func (m *Manager) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-m.jobs:
			m.deliver(ctx, j)
		}
	}
}

// deliver POSTs one job, retrying up to maxAttempts with linear backoff.
// On final failure it logs (surfacing on the Logs page) and records the
// failure for the status line.
func (m *Manager) deliver(ctx context.Context, j job) {
	var lastErr string
	var lastCode int
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		code, err := m.attempt(ctx, j)
		if err == nil && code >= 200 && code < 300 {
			m.recordSuccess(j.name, code)
			return
		}
		if err != nil {
			lastErr = err.Error()
			lastCode = 0
		} else {
			lastErr = fmt.Sprintf("HTTP %d", code)
			lastCode = code
		}
		if attempt < maxAttempts {
			// Linear backoff: 1s, then 2s. Abort early if shutting down.
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}
	}
	m.recordFailureCode(j.name, lastCode, lastErr)
	log.Printf("webhook %q: delivery to %s failed after %d attempts: %s",
		j.name, j.url, maxAttempts, lastErr)
}

// attempt performs a single POST. Returns the HTTP status code (0 on a
// transport-level error) and an error for transport failures only.
func (m *Manager) attempt(ctx context.Context, j job) (int, error) {
	reqCtx, cancel := context.WithTimeout(ctx, deliverTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, j.url, bytes.NewReader(j.body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "aprgo-webhook")
	if j.headerKey != "" {
		req.Header.Set(j.headerKey, j.headerVal)
	}
	client := m.clientVerify
	if j.insecure {
		client = m.clientInsecure
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	// Drain + close so the connection can be reused.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}

// SendTest delivers a single synthetic packet to wh and returns the HTTP
// status code (0 on transport error) and any transport error. No retry — the
// operator wants immediate pass/fail feedback. Does not touch status counters.
func (m *Manager) SendTest(wh state.Webhook) (int, error) {
	if strings.TrimSpace(wh.URL) == "" {
		return 0, fmt.Errorf("no URL configured")
	}
	body, err := buildBody(sampleTestPacket(), true)
	if err != nil {
		return 0, err
	}
	return m.attempt(context.Background(), job{
		url:       wh.URL,
		headerKey: wh.HeaderName,
		headerVal: wh.HeaderValue,
		insecure:  wh.InsecureSkipTLS,
		body:      body,
	})
}

// Snapshot returns a copy of the current per-endpoint status, keyed by
// webhook Name, for the Settings page.
func (m *Manager) Snapshot() map[string]EndpointStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]EndpointStatus, len(m.status))
	for k, v := range m.status {
		out[k] = *v
	}
	return out
}

func (m *Manager) entry(name string) *EndpointStatus {
	if m.status[name] == nil {
		m.status[name] = &EndpointStatus{}
	}
	return m.status[name]
}

func (m *Manager) recordSuccess(name string, code int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.entry(name)
	e.LastAttempt = time.Now()
	e.LastCode = code
	e.LastErr = ""
	e.Sent++
}

func (m *Manager) recordFailureCode(name string, code int, errText string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.entry(name)
	e.LastAttempt = time.Now()
	e.LastCode = code
	e.LastErr = errText
	e.Failed++
}

func (m *Manager) recordFailure(name, errText string) {
	m.recordFailureCode(name, 0, errText)
}
