package platform

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBurstFiresAtThreshold(t *testing.T) {
	c, err := OpenDenialCounter(filepath.Join(t.TempDir(), "denials.json"), time.Minute, 3)
	if err != nil {
		t.Fatalf("OpenDenialCounter: %v", err)
	}
	for i := 1; i <= 3; i++ {
		count, burst, err := c.Observe("agent-1", ClassShell)
		if err != nil {
			t.Fatalf("Observe: %v", err)
		}
		if count != i {
			t.Fatalf("count = %d, want %d", count, i)
		}
		if wantBurst := i >= 3; burst != wantBurst {
			t.Fatalf("burst at count %d = %v, want %v", i, burst, wantBurst)
		}
	}
}

// The pattern spans processes: a run that dies and resumes, or a script repeating
// a CLI call, looks like several well-behaved callers unless the count survives.
func TestBurstSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "denials.json")
	c, err := OpenDenialCounter(path, time.Minute, 3)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, _, err := c.Observe("agent-1", ClassShell); err != nil {
			t.Fatalf("observe: %v", err)
		}
	}

	reopened, err := OpenDenialCounter(path, time.Minute, 3)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	count, burst, err := reopened.Observe("agent-1", ClassShell)
	if err != nil {
		t.Fatalf("observe after reopen: %v", err)
	}
	if count != 3 || !burst {
		t.Fatalf("after restart count = %d, burst = %v; the pattern was lost", count, burst)
	}
}

// The whole point of keying on the class: a refused agent that renames the tool
// must land in the same bucket.
func TestReframeIncrementsBurst(t *testing.T) {
	c, err := OpenDenialCounter(filepath.Join(t.TempDir(), "denials.json"), time.Minute, 3)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// Two refusals of the same class, as if the agent renamed the tool between
	// them: the class, not the name, is what the counter keys on.
	for i := 0; i < 2; i++ {
		if _, _, err := c.Observe("agent-1", ClassShell); err != nil {
			t.Fatalf("observe: %v", err)
		}
	}
	count, burst, err := c.Observe("agent-1", ClassShell)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	if count != 3 || !burst {
		t.Fatalf("renaming the tool reset the counter: count=%d burst=%v", count, burst)
	}
}

func TestDifferentClassesCountSeparately(t *testing.T) {
	c, err := OpenDenialCounter(filepath.Join(t.TempDir(), "denials.json"), time.Minute, 3)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, _, err := c.Observe("agent-1", ClassShell); err != nil {
		t.Fatal(err)
	}
	count, burst, err := c.Observe("agent-1", ClassFileWrite)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 || burst {
		t.Fatalf("a different class inherited the shell count: count=%d burst=%v", count, burst)
	}
}

func TestStampsOutsideWindowExpire(t *testing.T) {
	path := filepath.Join(t.TempDir(), "denials.json")
	c, err := OpenDenialCounter(path, time.Nanosecond, 3)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, _, err := c.Observe("agent-1", ClassShell); err != nil {
			t.Fatalf("observe: %v", err)
		}
	}
	time.Sleep(2 * time.Millisecond)
	count, burst, err := c.Observe("agent-1", ClassShell)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	if count != 1 || burst {
		t.Fatalf("expired stamps were counted: count=%d burst=%v", count, burst)
	}
}

// A counter the governor cannot read must not read as empty: that is a reset an
// agent with write access to the state dir would engineer.
func TestCorruptCounterIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "denials.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenDenialCounter(path, time.Minute, 3); err == nil {
		t.Fatal("a corrupt counter opened as empty")
	}
}
