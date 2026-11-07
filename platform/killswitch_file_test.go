package platform

import (
	"os"
	"path/filepath"
	"testing"
)

// TestTrippedScopeNamesWhatFired matters because the denial reason is the only
// thing an operator reads afterwards: "kill switch tripped" is the same message
// whether one agent was stopped or the whole fleet was.
func TestTrippedScopeNamesWhatFired(t *testing.T) {
	k := NewKillSwitch()
	if got := k.TrippedScope("agent-7"); got != "" {
		t.Fatalf("untripped scope = %q, want empty", got)
	}
	k.Trip("agent-7")
	if got := k.TrippedScope("agent-7"); got != "agent-7" {
		t.Fatalf("scope = %q, want agent-7", got)
	}
	if got := k.TrippedScope("agent-8"); got != "" {
		t.Fatalf("other scope = %q, want empty", got)
	}
	k.Trip("global")
	if got := k.TrippedScope("agent-8"); got != "global" {
		t.Fatalf("global must win for untripped scopes, got %q", got)
	}
	if got := k.TrippedScope("agent-7"); got != "global" {
		t.Fatalf("global must win over a narrower trip, got %q", got)
	}
}

// TestFileKillSwitchSurvivesRestart is the whole point of the file-backed
// switch: a stop that ends with the process that declared it is not a stop.
func TestFileKillSwitchSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kill.json")

	first, err := OpenFileKillSwitch(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Trip("agent-7"); err != nil {
		t.Fatalf("trip: %v", err)
	}

	// A new process opens the same state directory.
	second, err := OpenFileKillSwitch(path)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Tripped("agent-7") {
		t.Fatal("trip did not survive reopening")
	}
	if second.Tripped("agent-8") {
		t.Fatal("trip leaked to an unrelated scope")
	}
}

func TestFileKillSwitchTripIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kill.json")
	k, err := OpenFileKillSwitch(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := k.Trip("global"); err != nil {
			t.Fatalf("trip %d: %v", i, err)
		}
	}
	scopes := k.Scopes()
	if len(scopes) != 1 || scopes[0] != "global" {
		t.Fatalf("scopes = %v, want [global]", scopes)
	}
}

// TestFileKillSwitchCreatesDirectory keeps the CLI honest: `--state ./fresh`
// must work on a directory that does not exist yet.
func TestFileKillSwitchCreatesDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "state", "kill.json")
	k, err := OpenFileKillSwitch(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := k.Trip("agent-1"); err != nil {
		t.Fatalf("trip: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("kill switch file not written: %v", err)
	}
}

// TestFileKillSwitchRefusesGarbage: an unreadable kill switch must not be
// treated as an open switch. Failing closed is the only safe reading.
func TestFileKillSwitchRefusesGarbage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kill.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenFileKillSwitch(path); err == nil {
		t.Fatal("a corrupt kill switch must not open as all-clear")
	}
}
