package main

import (
	"reflect"
	"testing"

	"github.com/aj-nt/gaap"
)

// TestApprovalFlagsAreOffByDefault: the loop exists so a run can be left
// attended, not so every run blocks on a person. Off unless asked for.
func TestApprovalFlagsAreOffByDefault(t *testing.T) {
	cfg, err := parseRunFlags("run", []string{"inventory the repo"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Approval {
		t.Error("approval should be off by default")
	}
	if cfg.ApprovalTimeoutSec != 900 {
		t.Errorf("default approval timeout = %d, want 900", cfg.ApprovalTimeoutSec)
	}
	if len(cfg.ApprovalAgents) != 0 {
		t.Errorf("default allowlist = %v, want empty (anyone)", cfg.ApprovalAgents)
	}
}

// TestApprovalFlagsAreParsed: the three flags reach the config.
func TestApprovalFlagsAreParsed(t *testing.T) {
	cfg, err := parseRunFlags("run", []string{
		"--approval", "--approval-timeout", "120", "--approval-agents", "aj, cait",
		"inventory the repo",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Approval {
		t.Error("--approval should enable the loop")
	}
	if cfg.ApprovalTimeoutSec != 120 {
		t.Errorf("timeout = %d, want 120", cfg.ApprovalTimeoutSec)
	}
	if want := []string{"aj", "cait"}; !reflect.DeepEqual(cfg.ApprovalAgents, want) {
		t.Errorf("allowlist = %v, want %v", cfg.ApprovalAgents, want)
	}
}

// TestWorkerPoolNeverClaimsApprovals: a pool that claimed the approval agent
// type would answer the orchestrator's own questions, which is the one thing
// the loop exists to prevent.
func TestWorkerPoolNeverClaimsApprovals(t *testing.T) {
	got := workerAgentTypes([]string{"quality_scan", gaap.ApprovalAgentType, "file_analysis"})
	if want := []string{"quality_scan", "file_analysis"}; !reflect.DeepEqual(got, want) {
		t.Errorf("worker types = %v, want %v", got, want)
	}

	// A list that is only the approval type leaves the pool with nothing to
	// claim, which is inert rather than dangerous.
	if got := workerAgentTypes([]string{gaap.ApprovalAgentType}); len(got) != 0 {
		t.Errorf("worker types = %v, want none", got)
	}
}
