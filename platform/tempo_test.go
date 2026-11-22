package platform

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTempo(t *testing.T) (*Tempo, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tempo.json")
	tp, err := OpenTempo(path, DefaultTempoWindow, 64)
	if err != nil {
		t.Fatalf("OpenTempo: %v", err)
	}
	return tp, path
}

// A single long pause between two bursts must not make the median look patient.
// That is the whole reason the measurement is a median.
func TestMedianGapIgnoresAnOutlier(t *testing.T) {
	tp, _ := newTempo(t)
	base := time.Now().Add(-time.Minute)
	// Four one-second gaps, then one thirty-second gap.
	for i := 0; i < 5; i++ {
		if err := tp.observeAt("agent-1", base.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tp.observeAt("agent-1", base.Add(35*time.Second)); err != nil {
		t.Fatal(err)
	}
	got, n := tp.MedianGap("agent-1")
	if n != 5 {
		t.Fatalf("gaps = %d, want 5", n)
	}
	if got != time.Second {
		t.Fatalf("median gap = %v, want 1s", got)
	}
}

func TestBurstDetectedOnCompressedSequence(t *testing.T) {
	tp, _ := newTempo(t)
	base := time.Now().Add(-time.Minute)
	// Five arrivals inside two seconds: inside the measured 2–25s window.
	for i := 0; i < 5; i++ {
		if err := tp.observeAt("agent-1", base.Add(time.Duration(i)*500*time.Millisecond)); err != nil {
			t.Fatal(err)
		}
	}
	if !tp.DetectBurst("agent-1") {
		t.Fatal("a two-second sequence of five calls was not flagged as a burst")
	}
}

func TestSpreadOutActivityIsNotABurst(t *testing.T) {
	tp, _ := newTempo(t)
	base := time.Now().Add(-4 * time.Minute)
	// Five arrivals over two minutes: same count, different shape.
	for i := 0; i < 5; i++ {
		if err := tp.observeAt("agent-1", base.Add(time.Duration(i)*30*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	if tp.DetectBurst("agent-1") {
		t.Fatal("a two-minute spread was flagged as a burst")
	}
}

func TestTooFewEventsIsNotABurst(t *testing.T) {
	tp, _ := newTempo(t)
	base := time.Now().Add(-time.Minute)
	for i := 0; i < BurstMinEvents-1; i++ {
		if err := tp.observeAt("agent-1", base.Add(time.Duration(i)*100*time.Millisecond)); err != nil {
			t.Fatal(err)
		}
	}
	if tp.DetectBurst("agent-1") {
		t.Fatal("a four-call sequence was flagged as a burst")
	}
}

// Six subjects in sorted order, seconds apart: enumeration rather than selection.
func TestSortedTraversalFlagged(t *testing.T) {
	subjects := []string{"tenant-0041", "tenant-0042", "tenant-0043", "tenant-0044", "tenant-0045", "tenant-0046"}
	gaps := []time.Duration{2 * time.Second, 3 * time.Second, 2 * time.Second, 4 * time.Second, 3 * time.Second}
	if !DetectSortedTraversal(subjects, gaps) {
		t.Fatal("sorted identifiers at three-second cadence were not flagged")
	}
}

// Sorted order alone is ordinary: a client that walks a list in order is not
// enumerating, it is working.
func TestSortedButSlowTraversalNotFlagged(t *testing.T) {
	subjects := []string{"a", "b", "c", "d", "e", "f"}
	gaps := []time.Duration{2 * time.Minute, 2 * time.Minute, 2 * time.Minute, 2 * time.Minute, 2 * time.Minute}
	if DetectSortedTraversal(subjects, gaps) {
		t.Fatal("a slow sorted traversal was flagged")
	}
}

// Machine cadence through unsorted identifiers is a different pattern; this
// detector must not claim it.
func TestUnsortedTraversalNotFlagged(t *testing.T) {
	subjects := []string{"tenant-0046", "tenant-0012", "tenant-0031", "tenant-0007", "tenant-0022", "tenant-0002"}
	gaps := []time.Duration{time.Second, time.Second, time.Second, time.Second, time.Second}
	if DetectSortedTraversal(subjects, gaps) {
		t.Fatal("an unsorted traversal was flagged as enumeration")
	}
}

func TestTooFewSubjectsNotFlagged(t *testing.T) {
	subjects := []string{"a", "b", "c"}
	gaps := []time.Duration{time.Second, time.Second}
	if DetectSortedTraversal(subjects, gaps) {
		t.Fatal("three identifiers were treated as a traversal")
	}
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
	if got := reopened.Count("agent-1"); got != 5 {
		t.Fatalf("count after restart = %d, want 5", got)
	}
	if !reopened.DetectBurst("agent-1") {
		t.Fatal("the burst was forgotten across the restart")
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
	if got := tp.Count("agent-1"); got != 1 {
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
	if got := tp.Count("agent-1"); got != 8 {
		t.Fatalf("count = %d, want the 8-sample bound", got)
	}
}
