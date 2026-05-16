package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// captureTarget is a tiny HTTP test target that records POSTs the relay
// makes to it. Lets us drive the full handle() loop without spinning up
// Slack.
type captureTarget struct {
	mu       sync.Mutex
	requests [][]byte
	status   int
}

func (c *captureTarget) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		c.mu.Lock()
		c.requests = append(c.requests, body)
		c.mu.Unlock()
		if c.status == 0 {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(c.status)
	}
}

func (c *captureTarget) last() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.requests) == 0 {
		return nil
	}
	return c.requests[len(c.requests)-1]
}

func newRelay(target string, format string) *relay {
	return &relay{
		client:    &http.Client{Timeout: 2 * time.Second},
		target:    target,
		format:    format,
		namespace: "kratix-platform-system",
		now:       func() time.Time { return time.Date(2026, 5, 15, 19, 0, 0, 0, time.UTC) },
		log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestRelay_GenericFormat(t *testing.T) {
	cap := &captureTarget{}
	tsrv := httptest.NewServer(cap.handler())
	defer tsrv.Close()

	r := newRelay(tsrv.URL, "generic")

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/events", bytes.NewReader([]byte(validProposed)))
	r.handle(resp, req)

	if resp.Code != http.StatusAccepted {
		t.Errorf("status = %d, want 202", resp.Code)
	}
	if got := cap.last(); got == nil {
		t.Fatal("target received no POST")
	}
	var env genericEnvelope
	if err := json.Unmarshal(cap.last(), &env); err != nil {
		t.Fatalf("target body not generic JSON: %v", err)
	}
	if env.ProposalID != "01HZ-PROP" {
		t.Errorf("proposalId = %q", env.ProposalID)
	}
}

func TestRelay_SlackFormat(t *testing.T) {
	cap := &captureTarget{}
	tsrv := httptest.NewServer(cap.handler())
	defer tsrv.Close()

	r := newRelay(tsrv.URL, "slack")

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/events", bytes.NewReader([]byte(validProposed)))
	r.handle(resp, req)
	if resp.Code != http.StatusAccepted {
		t.Errorf("status = %d", resp.Code)
	}
	var payload slackPayload
	if err := json.Unmarshal(cap.last(), &payload); err != nil {
		t.Fatalf("target body not slack JSON: %v", err)
	}
	if len(payload.Blocks) == 0 {
		t.Error("expected slack blocks")
	}
}

func TestRelay_RejectsBadBodyButAcks(t *testing.T) {
	// Forwarder retries on 4xx/5xx. The notifier deliberately acks
	// undeliverable events so a malformed upstream doesn't loop forever.
	cap := &captureTarget{}
	tsrv := httptest.NewServer(cap.handler())
	defer tsrv.Close()

	r := newRelay(tsrv.URL, "generic")
	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/events", bytes.NewReader([]byte("not json")))
	r.handle(resp, req)
	if resp.Code != http.StatusAccepted {
		t.Errorf("status = %d, want 202 (ack-and-drop)", resp.Code)
	}
	if cap.last() != nil {
		t.Errorf("malformed body should not be forwarded; target got %s", cap.last())
	}
}

func TestRelay_TargetErrorIsSwallowed(t *testing.T) {
	cap := &captureTarget{status: http.StatusServiceUnavailable}
	tsrv := httptest.NewServer(cap.handler())
	defer tsrv.Close()

	r := newRelay(tsrv.URL, "generic")
	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/events", bytes.NewReader([]byte(validProposed)))
	r.handle(resp, req)
	if resp.Code != http.StatusAccepted {
		t.Errorf("status = %d, want 202 even on target 5xx", resp.Code)
	}
}

func TestRelay_GET_Refused(t *testing.T) {
	r := newRelay("http://unused", "generic")
	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/events", nil)
	r.handle(resp, req)
	if resp.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET status = %d", resp.Code)
	}
}
