package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
)

// cloudEvent is just enough of the CloudEvents v1.0 envelope for the
// receiver to populate the window. The forwarder emits structured-mode
// JSON (eventing/pkg/schema.ContentType); we only read the fields we need.
type cloudEvent struct {
	Type     string `json:"type"`
	Subject  string `json:"subject"`
	Severity string `json:"kratixseverity"`
}

type receiver struct {
	window *window
	log    *slog.Logger
}

func newReceiver(w *window, log *slog.Logger) *receiver {
	return &receiver{window: w, log: log}
}

// Handler returns the HTTP handler wired into the Deployment's
// `/events` and `/healthz` paths.
func (r *receiver) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/events", r.handleEvent)
	return mux
}

func (r *receiver) handleEvent(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer req.Body.Close()
	body, err := io.ReadAll(io.LimitReader(req.Body, 1<<20)) // 1 MiB cap
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}

	var ce cloudEvent
	if err := json.Unmarshal(body, &ce); err != nil {
		r.log.Warn("malformed cloudevent body", "err", err)
		// Per CE HTTP binding: 400 on malformed payload.
		http.Error(w, "malformed cloudevent", http.StatusBadRequest)
		return
	}
	if ce.Type == "" || ce.Subject == "" {
		r.log.Warn("cloudevent missing required fields", "type", ce.Type, "subject", ce.Subject)
		http.Error(w, "missing type or subject", http.StatusBadRequest)
		return
	}

	r.window.Observe(ce.Subject, ce.Type, ce.Severity)
	w.WriteHeader(http.StatusAccepted)
}
