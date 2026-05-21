package server

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"sync"
	"time"

	"aprgo/internal/state"
	"aprgo/internal/tnc"
)

// pairJob tracks an in-progress Bluetooth Classic pairing operation. The
// wizard's TNC step kicks one off as a goroutine and returns immediately; the
// browser polls /setup/tnc/pair-status to render a live spinner / success /
// error state.
type pairJob struct {
	Addr      string
	Started   time.Time
	State     string // "running" | "ok" | "err"
	Err       string
	Channel   int
	Device    string
	cancel    context.CancelFunc
}

// pairStore is keyed by session cookie value.
type pairStore struct {
	mu   sync.Mutex
	jobs map[string]*pairJob
}

func newPairStore() *pairStore {
	return &pairStore{jobs: make(map[string]*pairJob)}
}

func (p *pairStore) get(key string) *pairJob {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.jobs[key]
}

func (p *pairStore) set(key string, j *pairJob) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.jobs[key] = j
}

func (p *pairStore) clear(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.jobs, key)
}

// gc removes finished or stale jobs older than 5 minutes.
func (p *pairStore) gc() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, j := range p.jobs {
		if time.Since(j.Started) > 5*time.Minute {
			delete(p.jobs, k)
		}
	}
}

// startBTPair launches the pair goroutine for a session and PRG-redirects
// to /setup/tnc/pairing so the full wizard page (with HTMX loaded) renders
// the polling spinner. Writing a bare fragment as the form-POST response
// replaced the entire document and broke HTMX — fixed by routing the user
// to a real page that includes the layout chrome.
func (s *Server) startBTPair(w http.ResponseWriter, r *http.Request, addr string) {
	key, ok := sessionKey(r)
	if !ok {
		http.Error(w, "no session", http.StatusForbidden)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	job := &pairJob{
		Addr:    addr,
		Started: time.Now(),
		State:   "running",
		cancel:  cancel,
	}
	s.pairs.set(key, job)
	go s.runPair(ctx, key, job)
	http.Redirect(w, r, "/setup/tnc/pairing", http.StatusSeeOther)
}

// handlePairingPage renders the in-progress pairing UI as a full wizard
// page so HTMX is loaded and the poll fragment can actually fire.
func (s *Server) handlePairingPage(w http.ResponseWriter, r *http.Request) {
	key, ok := sessionKey(r)
	if !ok {
		http.Redirect(w, r, "/login?next=/setup", http.StatusFound)
		return
	}
	job := s.pairs.get(key)
	d := s.draftFor(r, "")
	if d == nil || job == nil {
		http.Redirect(w, r, "/setup", http.StatusFound)
		return
	}
	data := s.common("Pairing", r)
	data["WizardLabel"] = wizardLabels[d.Flavor]
	data["StepIdx"] = d.StepIdx
	data["Steps"] = wizardStepTitles(wizardSteps[d.Flavor])
	data["StepKey"] = "tnc"
	data["StepTitle"] = "Pairing Bluetooth TNC"
	data["StepHelp"] = "This can take 20–30 seconds. The page will update automatically."
	data["St"] = d.Draft
	data["PrevURL"] = "/setup/back"
	data["HasPrev"] = false
	data["PairAddr"] = job.Addr
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	if err := s.tmpl.ExecuteTemplate(w, "setup.html", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// runPair executes the actual pair → channel-discovery → device-pick steps.
func (s *Server) runPair(ctx context.Context, key string, job *pairJob) {
	defer job.cancel()
	if err := tnc.Pair(ctx, job.Addr); err != nil {
		job.State = "err"
		job.Err = err.Error()
		return
	}
	job.Channel = tnc.DiscoverSPPChannel(ctx, job.Addr)
	// Reuse the existing rfcomm slot for this address if one is already
	// bound — otherwise repeat pair attempts accumulate /dev/rfcomm0,
	// rfcomm1, rfcomm2... each time. When all 32 slots are bound, surface
	// the recovery hint instead of silently overwriting an active binding.
	dev, derr := tnc.ChooseRFCOMMFor(job.Addr)
	if derr != nil {
		job.State = "err"
		job.Err = derr.Error()
		return
	}
	job.Device = dev
	job.State = "ok"
}

// handlePairStatus is the HTMX polling endpoint. Returns:
//   - "running" fragment → re-poll
//   - "ok" fragment → commits the pair result to the draft + shows Continue button
//   - "err" fragment → error message + retry option
func (s *Server) handlePairStatus(w http.ResponseWriter, r *http.Request) {
	key, ok := sessionKey(r)
	if !ok {
		http.Error(w, "no session", http.StatusForbidden)
		return
	}
	job := s.pairs.get(key)
	if job == nil {
		fmt.Fprintf(w, `<div class="dim">(no pair in progress)</div>`)
		return
	}
	switch job.State {
	case "running":
		elapsed := int(time.Since(job.Started).Seconds())
		fmt.Fprintf(w, `<div id="pair-progress" hx-get="/setup/tnc/pair-status" hx-trigger="every 1s" hx-swap="outerHTML">
  <div class="dim">🔵 Pairing with <code>%s</code>… (%ds)</div>
</div>`, html.EscapeString(job.Addr), elapsed)
	case "ok":
		// Commit to the wizard draft so the next step advances the wizard.
		d := s.draftFor(r, "")
		if d != nil {
			d.mu.Lock()
			d.Draft.TNCKind = state.TNCSerial
			d.Draft.TNCSerial = job.Device
			d.Draft.TNCAddr = job.Addr
			d.Draft.TNCChannel = job.Channel
			d.mu.Unlock()
		}
		s.pairs.clear(key)
		flavor := flavorFirstRun
		if d != nil {
			flavor = d.Flavor
		}
		csrf := s.csrfTokenFor(key)
		fmt.Fprintf(w, `<div class="flash ok">✓ Paired with <code>%s</code> on channel %d. Device: <code>%s</code></div>
<form method="POST" action="/setup/save/tnc-confirm" style="margin-top:12px;">
  <input type="hidden" name="csrf_token" value="%s">
  <input type="hidden" name="flavor" value="%s">
  <button type="submit" class="primary">Continue →</button>
</form>`,
			html.EscapeString(job.Addr), job.Channel, html.EscapeString(job.Device),
			html.EscapeString(csrf), html.EscapeString(string(flavor)))
	case "err":
		s.pairs.clear(key)
		fmt.Fprintf(w, `<div class="flash err">Pair failed: %s</div><div style="margin-top:8px;"><a href="/setup/%s" class="btn ghost">← Back to TNC picker</a></div>`,
			html.EscapeString(job.Err),
			html.EscapeString(string(flavorFirstRun)))
	}
}

// handleTNCConfirm is the user-clicked Continue button after a successful
// async pair. Advances the wizard to the next step.
func (s *Server) handleTNCConfirm(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if !s.requireCSRF(w, r) {
		return
	}
	d := s.draftFor(r, "")
	if d == nil {
		http.Redirect(w, r, "/setup", http.StatusFound)
		return
	}
	d.mu.Lock()
	steps := wizardSteps[d.Flavor]
	d.StepIdx++
	if d.StepIdx >= len(steps) {
		d.StepIdx = len(steps) - 1
	}
	d.Modified = time.Now()
	// If we've landed on the wizard's "done" step, commit the draft. Without
	// this the BT-pair flow ends at "done" but never persists state — the
	// runtime keeps reading the *old* TNC config and the radio status banner
	// shows a stale disconnect.
	var commitErr error
	if d.StepIdx < len(steps) && steps[d.StepIdx] == "done" {
		commitErr = s.commitWizardDraft(d)
	}
	d.mu.Unlock()
	if commitErr != nil {
		http.Error(w, commitErr.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/setup/"+string(d.Flavor), http.StatusFound)
}
