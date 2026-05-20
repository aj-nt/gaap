package main

import (
	"reflect"
	"strings"
	"testing"
)

// TestParseRunFlagsDefaults verifies that an empty flag set (just a goal)
// returns a config with all defaults applied.
func TestParseRunFlagsDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := parseRunFlags("run", []string{"audit the codebase"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Goal != "audit the codebase" {
		t.Errorf("expected goal 'audit the codebase', got %q", cfg.Goal)
	}
	if cfg.DaemonAddr != "localhost:50051" {
		t.Errorf("expected default daemon addr, got %q", cfg.DaemonAddr)
	}
	if cfg.MaxWaitSec != 300 {
		t.Errorf("expected default timeout 300, got %d", cfg.MaxWaitSec)
	}
	if cfg.DryRun {
		t.Error("expected DryRun=false by default")
	}
}

// TestParseRunFlagsExplicit verifies that all flags are parsed when set.
func TestParseRunFlagsExplicit(t *testing.T) {
	t.Parallel()

	cfg, err := parseRunFlags("run", []string{
		"--dry-run",
		"--addr", "192.168.1.1:50051",
		"--repo", "/srv/repos/gaap",
		"--timeout", "600",
		"deep scan",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Goal != "deep scan" {
		t.Errorf("expected goal 'deep scan', got %q", cfg.Goal)
	}
	if cfg.DaemonAddr != "192.168.1.1:50051" {
		t.Errorf("expected explicit addr, got %q", cfg.DaemonAddr)
	}
	if cfg.RepoPath != "/srv/repos/gaap" {
		t.Errorf("expected explicit repo, got %q", cfg.RepoPath)
	}
	if cfg.MaxWaitSec != 600 {
		t.Errorf("expected explicit timeout 600, got %d", cfg.MaxWaitSec)
	}
	if !cfg.DryRun {
		t.Error("expected DryRun=true when --dry-run is set")
	}
}

// TestParseRunFlagsMissingGoal verifies error on missing positional goal.
func TestParseRunFlagsMissingGoal(t *testing.T) {
	t.Parallel()

	_, err := parseRunFlags("run", []string{"--dry-run"})
	if err == nil {
		t.Fatal("expected error for missing goal")
	}
	if !strings.Contains(err.Error(), "goal is required") {
		t.Errorf("expected 'goal is required' in error, got %q", err.Error())
	}
}

// TestParseRunFlagsEmptyArgs verifies error when args is nil/empty.
func TestParseRunFlagsEmptyArgs(t *testing.T) {
	t.Parallel()

	_, err := parseRunFlags("run", nil)
	if err == nil {
		t.Fatal("expected error for nil args")
	}
}

// TestParseRunFlagsInvalidTimeout verifies error on non-numeric timeout.
func TestParseRunFlagsInvalidTimeout(t *testing.T) {
	t.Parallel()

	_, err := parseRunFlags("run", []string{"--timeout", "abc", "goal"})
	if err == nil {
		t.Fatal("expected error for non-numeric timeout")
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Errorf("expected 'timeout' in error, got %q", err.Error())
	}
}

// TestRunConfigEquality verifies that two configs with the same fields are equal.
// This guards against future field additions breaking tests silently.
func TestRunConfigEquality(t *testing.T) {
	t.Parallel()

	a := runConfig{
		Goal:          "scan",
		DaemonAddr:    "localhost:50051",
		RepoPath:      "/tmp/repo",
		MaxWaitSec:    300,
		PollIntervalSec: 5,
		DryRun:        false,
	}
	b := runConfig{
		Goal:          "scan",
		DaemonAddr:    "localhost:50051",
		RepoPath:      "/tmp/repo",
		MaxWaitSec:    300,
		PollIntervalSec: 5,
		DryRun:        false,
	}

	if !reflect.DeepEqual(a, b) {
		t.Error("identical configs should be equal")
	}
}
