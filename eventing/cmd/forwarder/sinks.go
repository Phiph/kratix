package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/syntasso/kratix/eventing/pkg/schema"
)

// deliverer fans a CloudEvent out to every matching sink. v0.1: synchronous,
// no retries beyond the per-request HTTP timeout. Non-2xx responses are
// logged and counted, never retried. This matches the wire-format doc's
// drop-and-count discipline.
type deliverer struct {
	client *http.Client
	log    *slog.Logger
}

func newDeliverer(timeout time.Duration, log *slog.Logger) *deliverer {
	return &deliverer{
		client: &http.Client{Timeout: timeout},
		log:    log,
	}
}

func (d *deliverer) fanOut(ctx context.Context, ce *cloudEvent, sinks []sinkSnapshot) {
	body, err := json.Marshal(ce)
	if err != nil {
		d.log.Error("failed to marshal cloudevent", "err", err, "type", ce.Type)
		return
	}
	for _, sink := range sinks {
		if !sink.matches(ce.Type) {
			continue
		}
		d.send(ctx, sink, body, ce)
	}
}

func (d *deliverer) send(ctx context.Context, sink sinkSnapshot, body []byte, ce *cloudEvent) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sink.url, bytes.NewReader(body))
	if err != nil {
		d.log.Error("build request failed", "sink", sink.name, "err", err)
		return
	}
	req.Header.Set("Content-Type", schema.ContentType)

	resp, err := d.client.Do(req)
	if err != nil {
		d.log.Warn("sink POST failed", "sink", sink.name, "url", sink.url, "type", ce.Type, "err", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		d.log.Warn("sink non-2xx", "sink", sink.name, "status", resp.StatusCode, "type", ce.Type)
		return
	}
	d.log.Debug("delivered", "sink", sink.name, "type", ce.Type, "id", ce.ID)
	_ = fmt.Sprintf // keep import light if logger fmt changes
}
