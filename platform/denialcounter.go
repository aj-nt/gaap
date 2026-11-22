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

// Defaults for the refusal counter. Three within ten minutes is a pattern; two
// is a mistake, and every agent that probes a boundary once would trip a lower
// threshold.
const (
	DefaultDenialThreshold = 3
	DefaultDenialWindow    = 10 * time.Minute
)

// DenialCounter is a durable count of recent refusals per (agent, action class).
//
// It is keyed on the class, not the tool name, because the name is authored by
// the observed party: an agent refused at Execute retries as Shell, Run or Bash,
// and a counter keyed on the name resets on the first reframe. The class is what
// the standing orders say the call is FOR, so a reframed retry lands in the same
// bucket.
//
// Durable because the interesting pattern spans process boundaries: a governed
// run that dies and resumes, or a CLI invocation repeated by a script, is exactly
// how a patient prober looks like several well-behaved callers.
type DenialCounter struct {
	mu        sync.Mutex
	path      string
	window    time.Duration
	threshold int
	hits      map[string][]time.Time
}

// denialCounterFormat is the on-disk shape. Unix nanos rather than RFC3339: the
// file is machine state, not a record to be read.
type denialCounterFormat struct {
	WindowSeconds int                `json:"window_seconds"`
	Threshold     int                `json:"threshold"`
	Hits          map[string][]int64 `json:"hits"`
}

// OpenDenialCounter opens or creates the counter at path. A non-positive window
// or threshold takes the default rather than creating a counter that can never
// fire.
func OpenDenialCounter(path string, window time.Duration, threshold int) (*DenialCounter, error) {
	if window <= 0 {
		window = DefaultDenialWindow
	}
	if threshold <= 0 {
		threshold = DefaultDenialThreshold
	}
	c := &DenialCounter{
		path:      path,
		window:    window,
		threshold: threshold,
		hits:      make(map[string][]time.Time),
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return c, nil
		}
		return nil, err
	}
	if len(raw) == 0 {
		return c, nil
	}
	var stored denialCounterFormat
	if err := json.Unmarshal(raw, &stored); err != nil {
		// A corrupt counter must not silently reset: a reset is exactly what an
		// agent that can write the state dir would want.
		return nil, fmt.Errorf("denial counter %s is not readable: %w", path, err)
	}
	cutoff := time.Now().Add(-window)
	for key, stamps := range stored.Hits {
		for _, ns := range stamps {
			t := time.Unix(0, ns)
			if t.After(cutoff) {
				c.hits[key] = append(c.hits[key], t)
			}
		}
	}
	return c, nil
}

// Window reports the configured window, for messages.
func (c *DenialCounter) Window() time.Duration { return c.window }

// Threshold reports the configured threshold, for messages.
func (c *DenialCounter) Threshold() int { return c.threshold }

// DenialKey names the bucket a refusal counts against. An unclassified call
// shares one bucket with every other unclassified call: still a pattern, and
// better than a bucket per tool name.
func DenialKey(agentID string, class ActionClass) string {
	name := string(class)
	if name == "" {
		name = "unclassified"
	}
	return agentID + "|" + name
}

// Observe records a refusal and reports the count inside the window, and whether
// that count has reached the threshold.
//
// The write is synchronous and its error propagates: a counter that reported a
// burst it could not persist would forget the pattern on the next invocation,
// which is the case the durability exists for.
func (c *DenialCounter) Observe(agentID string, class ActionClass) (count int, burst bool, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	c.pruneLocked(now)

	key := DenialKey(agentID, class)
	c.hits[key] = append(c.hits[key], now)
	count = len(c.hits[key])
	burst = count >= c.threshold

	if err := c.persistLocked(); err != nil {
		return count, burst, err
	}
	return count, burst, nil
}

// pruneLocked drops stamps outside the window so the file stays bounded and a
// counter never accumulates history it will not use.
func (c *DenialCounter) pruneLocked(now time.Time) {
	cutoff := now.Add(-c.window)
	for key, stamps := range c.hits {
		kept := stamps[:0]
		for _, t := range stamps {
			if t.After(cutoff) {
				kept = append(kept, t)
			}
		}
		if len(kept) == 0 {
			delete(c.hits, key)
			continue
		}
		c.hits[key] = kept
	}
}

// persistLocked writes the counter atomically: temp file, fsync, rename. A crash
// mid-write leaves the previous count intact rather than a truncated one that
// reads as zero.
func (c *DenialCounter) persistLocked() error {
	stored := denialCounterFormat{
		WindowSeconds: int(c.window / time.Second),
		Threshold:     c.threshold,
		Hits:          make(map[string][]int64, len(c.hits)),
	}
	for key, stamps := range c.hits {
		ns := make([]int64, 0, len(stamps))
		for _, t := range stamps {
			ns = append(ns, t.UnixNano())
		}
		stored.Hits[key] = ns
	}
	b, err := json.Marshal(stored)
	if err != nil {
		return err
	}
	dir := filepath.Dir(c.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".denials-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }() // no-op after a successful rename
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
	if err := os.Rename(name, c.path); err != nil {
		return err
	}
	return syncDir(dir)
}
