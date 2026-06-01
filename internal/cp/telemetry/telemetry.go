// Package telemetry emits content-free session-lifecycle events. It never
// carries user /data content or relay-frame bytes — metadata only.
package telemetry

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

type Event struct {
	Kind      string    `json:"kind"` // spawn_create | session_start | session_end
	Owner     string    `json:"owner"`
	AppID     string    `json:"app_id"`
	Tier      string    `json:"tier"`
	Storage   string    `json:"storage"`
	NodeID    string    `json:"node_id"`
	NodeClass string    `json:"node_class"`
	SpawnID   string    `json:"spawn_id"`
	Timestamp time.Time `json:"ts"`
}

type Sink interface{ Emit(Event) error }

// NopSink discards events (tests/dev).
type NopSink struct{}

func (NopSink) Emit(Event) error { return nil }

// JSONLSink appends one JSON object per line. Concurrency-safe.
type JSONLSink struct {
	mu sync.Mutex
	f  *os.File
}

func NewJSONLSink(path string) (*JSONLSink, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &JSONLSink{f: f}, nil
}

func (s *JSONLSink) Emit(ev Event) error {
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.f.Write(append(b, '\n')); err != nil {
		return err
	}
	return nil
}

func (s *JSONLSink) Close() error { return s.f.Close() }
