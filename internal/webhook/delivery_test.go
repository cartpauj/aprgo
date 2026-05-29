package webhook

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"aprgo/internal/state"
)

// TestSendTestPostsValidPayload verifies a single delivery: method, headers,
// and a well-formed JSON body carrying the test flag.
func TestSendTestPostsValidPayload(t *testing.T) {
	var gotAuth, gotCT string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	m := New(nil, nil)
	code, err := m.SendTest(state.Webhook{
		Name:        "t",
		URL:         srv.URL,
		HeaderName:  "Authorization",
		HeaderValue: "Bearer secret",
	})
	if err != nil {
		t.Fatalf("SendTest: %v", err)
	}
	if code != http.StatusNoContent {
		t.Errorf("code = %d, want 204", code)
	}
	if gotAuth != "Bearer secret" {
		t.Errorf("auth header = %q", gotAuth)
	}
	if gotCT != "application/json" {
		t.Errorf("content-type = %q", gotCT)
	}
	var pl payload
	if err := json.Unmarshal(gotBody, &pl); err != nil {
		t.Fatalf("body not valid JSON: %v", err)
	}
	if !pl.Test {
		t.Error("test payload should set test=true")
	}
}

// TestDeliverSuccess confirms a 2xx is counted as sent on the first try.
func TestDeliverSuccess(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m := New(nil, nil)
	m.deliver(context.Background(), job{name: "ok", url: srv.URL, body: []byte("{}")})

	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("requests = %d, want 1 (no retry on success)", got)
	}
	st := m.Snapshot()["ok"]
	if st.Sent != 1 || st.Failed != 0 || st.LastCode != 200 {
		t.Errorf("status = %+v, want Sent=1 Failed=0 LastCode=200", st)
	}
}

// TestDeliverRetriesThenDrops confirms a persistently-failing endpoint is
// tried exactly maxAttempts times, then dropped and recorded as failed.
func TestDeliverRetriesThenDrops(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	m := New(nil, nil)
	m.deliver(context.Background(), job{name: "bad", url: srv.URL, body: []byte("{}")})

	if got := atomic.LoadInt32(&hits); got != maxAttempts {
		t.Errorf("requests = %d, want %d", got, maxAttempts)
	}
	st := m.Snapshot()["bad"]
	if st.Failed != 1 || st.Sent != 0 || st.LastCode != 500 {
		t.Errorf("status = %+v, want Failed=1 Sent=0 LastCode=500", st)
	}
}
