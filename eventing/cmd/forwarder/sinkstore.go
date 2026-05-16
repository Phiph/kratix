package main

import (
	"strings"
	"sync"

	eventingv1alpha1 "github.com/syntasso/kratix/eventing/api/v1alpha1"
)

// sinkSnapshot is an immutable view of one CloudEventSink: enough to deliver
// to it without dereferencing the live cache (which would race with informer
// updates).
type sinkSnapshot struct {
	name       string
	url        string
	typeFilter []string
}

func (s sinkSnapshot) matches(ceType string) bool {
	if len(s.typeFilter) == 0 {
		return true
	}
	for _, pat := range s.typeFilter {
		if matchGlob(pat, ceType) {
			return true
		}
	}
	return false
}

// matchGlob supports exact match and a single trailing "*" wildcard.
// "kratix.promise.*" matches "kratix.promise.unavailable".
// "kratix.promise.unavailable" matches only itself.
func matchGlob(pattern, s string) bool {
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(s, strings.TrimSuffix(pattern, "*"))
	}
	return pattern == s
}

// sinkStore is the forwarder's in-memory map of CloudEventSink CRs. It is
// updated by the informer event handlers (Add/Update/Delete) and read by the
// Event handler when fanning out deliveries.
type sinkStore struct {
	mu    sync.RWMutex
	sinks map[string]sinkSnapshot
}

func newSinkStore() *sinkStore {
	return &sinkStore{sinks: map[string]sinkSnapshot{}}
}

func (s *sinkStore) upsert(cs *eventingv1alpha1.CloudEventSink) {
	snap := sinkSnapshot{
		name:       cs.Name,
		url:        cs.Spec.URL,
		typeFilter: append([]string(nil), cs.Spec.TypeFilter...),
	}
	s.mu.Lock()
	s.sinks[cs.Name] = snap
	s.mu.Unlock()
}

func (s *sinkStore) delete(name string) {
	s.mu.Lock()
	delete(s.sinks, name)
	s.mu.Unlock()
}

// snapshot returns the current sink list. Returned slice is safe to iterate
// without holding the store lock.
func (s *sinkStore) snapshot() []sinkSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]sinkSnapshot, 0, len(s.sinks))
	for _, snap := range s.sinks {
		out = append(out, snap)
	}
	return out
}
