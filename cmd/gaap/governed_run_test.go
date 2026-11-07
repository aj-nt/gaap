package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestPolicyFlagsReachTheRunConfig: a governed run is governed by the flag, not
// by an environment that happens to be set.
func TestPolicyFlagsReachTheRunConfig(t *testing.T) {
	cfg, err := parseRunFlags("run", []string{"--policy", "rules.json", "--state", "/tmp/g", "--trust-floor", "600", "inventory"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Policy != "rules.json" {
		t.Errorf("policy = %q", cfg.Policy)
	}
	if cfg.StateDir != "/tmp/g" {
		t.Errorf("state = %q", cfg.StateDir)
	}
	if cfg.TrustFloor != 600 {
		t.Errorf("trust floor = %v", cfg.TrustFloor)
	}
}

// TestUngovernedByDefault: no --policy means no governor. A run that is governed
// by accident is a run nobody can explain.
func TestUngovernedByDefault(t *testing.T) {
	cfg, err := parseRunFlags("run", []string{"inventory"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Policy != "" {
		t.Errorf("policy = %q, want empty", cfg.Policy)
	}
}

// TestBuildGovernorRecordsToTheStateDirectory: the point of --state is that a
// decision made during a run is in the same record a person can verify later.
func TestBuildGovernorRecordsToTheStateDirectory(t *testing.T) {
	dir := t.TempDir()
	policyPath := filepath.Join(dir, "policy.json")
	policy := `{"version":1,"rules":[{"id":"allow-scan","tool":"static_analysis","action":"allow"}]}`
	if err := os.WriteFile(policyPath, []byte(policy), 0o600); err != nil {
		t.Fatal(err)
	}

	g, state, err := buildGovernor(policyPath, filepath.Join(dir, "state"), 0, "")
	if err != nil {
		t.Fatalf("buildGovernor: %v", err)
	}
	g.Decide("gaap-orchestrator", "static_analysis", map[string]any{"task_id": "abc_scan"})
	entries := state.ledger.Entries()
	if len(entries) != 1 {
		t.Fatalf("ledger entries = %d, want 1", len(entries))
	}
	if entries[0].Tool != "static_analysis" {
		t.Errorf("tool = %q", entries[0].Tool)
	}
	if err := state.Close(); err != nil {
		t.Fatalf("closing the state: %v", err)
	}

	// Reopening the same directory must show the record: a ledger that only
	// exists in memory is not a record.
	again, err := openGovernState(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatalf("reopening: %v", err)
	}
	defer func() { _ = again.Close() }()
	if got := len(again.ledger.Entries()); got != 1 {
		t.Errorf("reopened ledger has %d entries, want 1", got)
	}
}

// TestBuildGovernorRefusesAMissingPolicy: a run told to be governed but pointed
// at no policy would be governed by nothing, silently.
func TestBuildGovernorRefusesAMissingPolicy(t *testing.T) {
	if _, _, err := buildGovernor("", t.TempDir(), 0, ""); err == nil {
		t.Fatal("a governor with no policy should be refused")
	}
}
