package platform

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// retained counts the samples the store still holds for a subject. The exported
// accessors went with the detectors that used them: only Observe is wired, so a
// Count method would be a public reader nothing calls. The tests still need to
// see the store, and they are in-package.
func retained(tp *Tempo, subject string) int {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	return len(tp.arrival[subject])
}

func newTempo(t *testing.T) (*Tempo, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tempo.json")
	tp, err := OpenTempo(path, DefaultTempoWindow, 64)
	if err != nil {
		t.Fatalf("OpenTempo: %v", err)
	}
	return tp, path
}

// Timing is what a rename cannot change, so it must survive the process the way
// the refusal counter does.
func TestTempoSurvivesRestart(t *testing.T) {
	tp, path := newTempo(t)
	base := time.Now().Add(-30 * time.Second)
	for i := 0; i < 5; i++ {
		if err := tp.observeAt("agent-1", base.Add(time.Duration(i)*500*time.Millisecond)); err != nil {
			t.Fatal(err)
		}
	}
	reopened, err := OpenTempo(path, DefaultTempoWindow, 64)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := retained(reopened, "agent-1"); got != 5 {
		t.Fatalf("count after restart = %d, want 5", got)
	}
}

// A tempo file that cannot be parsed must not read as "nothing unusual".
func TestCorruptTempoIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tempo.json")
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenTempo(path, DefaultTempoWindow, 64); err == nil {
		t.Fatal("a corrupt tempo store opened as empty")
	}
}

// Samples outside the window are dropped, so an old burst does not colour today.
func TestSamplesOutsideWindowExpire(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tempo.json")
	tp, err := OpenTempo(path, time.Minute, 64)
	if err != nil {
		t.Fatal(err)
	}
	if err := tp.observeAt("agent-1", time.Now().Add(-10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	// A fresh observation prunes the stale one.
	if err := tp.Observe("agent-1"); err != nil {
		t.Fatal(err)
	}
	if got := retained(tp, "agent-1"); got != 1 {
		t.Fatalf("count = %d, want 1 after the stale sample expired", got)
	}
}

// Retention is bounded: the observed party writes these samples.
func TestSamplesAreBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tempo.json")
	tp, err := OpenTempo(path, DefaultTempoWindow, 8)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		if err := tp.observeAt("agent-1", time.Now().Add(-time.Duration(50-i)*time.Millisecond)); err != nil {
			t.Fatal(err)
		}
	}
	if got := retained(tp, "agent-1"); got != 8 {
		t.Fatalf("count = %d, want the 8-sample bound", got)
	}
}
