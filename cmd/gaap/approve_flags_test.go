package main

import (
	"strings"
	"testing"
)

// The flags are parsed by the command that dials a daemon, so a mistake here is
// only visible from a terminal — which is exactly how three of them shipped:
// no default daemon address, the flag parser giving up at the positional, and
// an answer accepted with no author.

// TestParseApprovalsFlagsTakesTheRunIDAfterFlags: Go's flag package stops at the
// first positional, so `approvals <run-id> --json` used to ignore --json.
func TestParseApprovalsFlagsTakesTheRunIDAfterFlags(t *testing.T) {
	cfg, err := parseApprovalsFlags([]string{"abc123", "--json"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.RunID != "abc123" {
		t.Errorf("run id = %q", cfg.RunID)
	}
	if !cfg.JSON {
		t.Error("--json after the run id was ignored")
	}
	if cfg.DaemonAddr == "" {
		t.Error("no default daemon address: every invocation would need --addr")
	}
	if _, err := parseApprovalsFlags(nil); err == nil {
		t.Error("a missing run id should be a usage error")
	}
}

// TestParseApproveFlagsCarriesTheAnswer: the parse reads the answer; whether it
// is answerable is decided once, in the command, by the same rule that gates
// every other refusal (and pinned there by TestApproveRequiresAReason). Checking
// it twice would let the two copies drift.
func TestParseApproveFlagsCarriesTheAnswer(t *testing.T) {
	cfg, err := parseApproveFlags([]string{"abc123", "--action", "retry", "--reason", "because", "--agent", "aj"})
	if err != nil {
		t.Fatalf("a complete answer was refused: %v", err)
	}
	if cfg.RunID != "abc123" || cfg.Action != "retry" || cfg.Agent != "aj" || cfg.Reason != "because" {
		t.Errorf("cfg = %+v", cfg)
	}
	if cfg.DaemonAddr == "" {
		t.Error("no default daemon address")
	}
	if _, err := parseApproveFlags([]string{"--action", "retry"}); err == nil {
		t.Error("a missing run id should be a usage error")
	}
}

// TestApproveFlagsAfterTheRunID: the documented form puts the run id first, so
// every flag after it has to survive the parse.
func TestApproveFlagsAfterTheRunID(t *testing.T) {
	cfg, err := parseApproveFlags([]string{"abc123", "--action", "edit", "--context", `{"path":"x.go"}`, "--reason", "fixed the path", "--agent", "aj"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Action != "edit" || !strings.Contains(cfg.Context, "x.go") {
		t.Errorf("cfg = %+v", cfg)
	}
}

// TestReorderArgsKeepsFlagValuesAttached: a reorder that loses the "--flag value"
// pairing turns a value into a positional, and the parse then fails with a
// message about the wrong thing.
func TestReorderArgsKeepsFlagValuesAttached(t *testing.T) {
	got := reorderArgs([]string{"run-1", "--addr", "localhost:50051", "--json"}, map[string]bool{"addr": true})
	want := "--addr localhost:50051 --json run-1"
	if strings.Join(got, " ") != want {
		t.Errorf("reorderArgs = %v, want %q", got, want)
	}

	got = reorderArgs([]string{"run-1", "--addr=localhost:50051"}, map[string]bool{"addr": true})
	if strings.Join(got, " ") != "--addr=localhost:50051 run-1" {
		t.Errorf("a --flag=value lost its value: %v", got)
	}

	// A boolean flag takes no value, so the word after it stays positional.
	got = reorderArgs([]string{"run-1", "--json"}, map[string]bool{"addr": true})
	if strings.Join(got, " ") != "--json run-1" {
		t.Errorf("reorderArgs = %v", got)
	}
}

// TestSplitList: the allowlist and the agent-type lists are comma-separated on
// the command line, and an empty entry is not a member.
func TestSplitList(t *testing.T) {
	if got := splitList(" aj, mary ,, "); strings.Join(got, "|") != "aj|mary" {
		t.Errorf("splitList = %v", got)
	}
	if got := splitList("   "); got != nil {
		t.Errorf("splitList(\"   \") = %v, want nil", got)
	}
}
