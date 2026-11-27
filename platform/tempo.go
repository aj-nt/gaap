package platform

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// DefaultTempoWindow is how far back tempo is measured. Ten minutes: long enough
// that a spray burst and the pause after it are in the same sample, short enough
// that yesterday's diligence does not flatter today's hammering.
const DefaultTempoWindow = 10 * time.Minute

// Tempo observes HOW a subject calls, not what it calls.
//
// The distinction matters because the tool name is authored by the observed
// party: an agent refused at Execute retries as Shell, Run or Bash, and every
// name-keyed measurement resets. Arrival timing and ordering are properties of
// the caller that a rename cannot change.
//
// Nothing here changes a verdict. Tempo produces observations; a rule that acts on
// them references them explicitly, so "the governor noticed you were fast" and
// "the governor refused you for being fast" stay separate claims.
//
// Durable, like the refusal counter: a burst that spans two processes is exactly
// what a patient prober looks like when timing is measured per-process.
type Tempo struct {
	mu      sync.Mutex
	path    string
	window  time.Duration
	maxPer  int // bound on retained samples per subject
	arrival map[string][]time.Time
}

type tempoFormat struct {
	WindowSeconds int                `json:"window_seconds"`
	Arrivals      map[string][]int64 `json:"arrivals"` // unix nanos
}

// OpenTempo opens or creates the tempo store at path.
//
// maxSamples bounds what is kept per subject. A governor that retains every
// arrival is an unbounded file written by the observed party, which is a denial
// of service with extra steps.
func OpenTempo(path string, window time.Duration, maxSamples int) (*Tempo, error) {
	if window <= 0 {
		window = DefaultTempoWindow
	}
	if maxSamples <= 0 {
		maxSamples = 256
	}
	t := &Tempo{
		path:    path,
		window:  window,
		maxPer:  maxSamples,
		arrival: make(map[string][]time.Time),
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return t, nil
		}
		return nil, err
	}
	if len(raw) == 0 {
		return t, nil
	}
	var stored tempoFormat
	if err := json.Unmarshal(raw, &stored); err != nil {
		// A corrupt tempo file must not silently reset to "nothing unusual":
		// that is the reading a subject who can write the state dir would engineer.
		return nil, fmt.Errorf("tempo store %s is not readable: %w", path, err)
	}
	cutoff := time.Now().Add(-window)
	for subject, stamps := range stored.Arrivals {
		for _, ns := range stamps {
			if ts := time.Unix(0, ns); ts.After(cutoff) {
				t.arrival[subject] = append(t.arrival[subject], ts)
			}
		}
	}
	return t, nil
}

// Window reports the measurement window.
func (t *Tempo) Window() time.Duration { return t.window }

// Observe records one arrival and persists. The error propagates: a tempo record
// that silently failed to write would measure a history it cannot prove.
func (t *Tempo) Observe(subject string) error { return t.observeAt(subject, time.Now()) }

// observeAt is the clock-injected form. Tests need it because a burst is defined
// by span, and a loop of real observations always spans microseconds.
func (t *Tempo) observeAt(subject string, at time.Time) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.pruneLocked(at)
	t.arrival[subject] = append(t.arrival[subject], at)
	if len(t.arrival[subject]) > t.maxPer {
		t.arrival[subject] = t.arrival[subject][len(t.arrival[subject])-t.maxPer:]
	}
	return t.persistLocked()
}

func (t *Tempo) pruneLocked(now time.Time) {
	cutoff := now.Add(-t.window)
	for subject, stamps := range t.arrival {
		kept := stamps[:0]
		for _, ts := range stamps {
			if ts.After(cutoff) {
				kept = append(kept, ts)
			}
		}
		if len(kept) == 0 {
			delete(t.arrival, subject)
			continue
		}
		t.arrival[subject] = kept
	}
}

func (t *Tempo) persistLocked() error {
	stored := tempoFormat{
		WindowSeconds: int(t.window / time.Second),
		Arrivals:      make(map[string][]int64, len(t.arrival)),
	}
	for subject, stamps := range t.arrival {
		ns := make([]int64, 0, len(stamps))
		for _, ts := range stamps {
			ns = append(ns, ts.UnixNano())
		}
		stored.Arrivals[subject] = ns
	}
	b, err := json.Marshal(stored)
	if err != nil {
		return err
	}
	dir := filepath.Dir(t.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tempo-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, t.path); err != nil {
		return err
	}
	return syncDir(dir)
}
