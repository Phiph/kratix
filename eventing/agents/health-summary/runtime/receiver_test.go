package main

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestReceiver_ValidEventLandsInWindow(t *testing.T) {
	w := newWindow(time.Hour)
	w.now = func() time.Time { return time.Date(2026, 5, 15, 19, 0, 0, 0, time.UTC) }
	r := newReceiver(w, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()

	body := `{
		"specversion": "1.0",
		"type": "kratix.promise.unavailable",
		"subject": "default/promise/redis",
		"kratixseverity": "warning"
	}`
	resp, err := http.Post(srv.URL+"/events", "application/cloudevents+json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	d := w.Snapshot()
	if d.Totals["warning"] != 1 {
		t.Errorf("warning total = %d", d.Totals["warning"])
	}
}

func TestReceiver_Healthz(t *testing.T) {
	w := newWindow(time.Hour)
	r := newReceiver(w, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz status = %d", resp.StatusCode)
	}
}

func TestReceiver_RejectsMalformed(t *testing.T) {
	w := newWindow(time.Hour)
	r := newReceiver(w, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()

	cases := []struct {
		name string
		body string
		want int
	}{
		{"not-json", `not json at all`, http.StatusBadRequest},
		{"missing-type", `{"subject":"x"}`, http.StatusBadRequest},
		{"missing-subject", `{"type":"x"}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := http.Post(srv.URL+"/events", "application/cloudevents+json", bytes.NewBufferString(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}

func TestReceiver_RejectsGET(t *testing.T) {
	w := newWindow(time.Hour)
	r := newReceiver(w, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /events status = %d, want 405", resp.StatusCode)
	}
}
