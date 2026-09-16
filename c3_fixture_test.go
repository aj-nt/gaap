// This file is part of Vagent.
// See LICENSE-Apache-2.0 for license information.

package gaap

// Fixture-integrity test for the C3 probe (spikes/004): the probe's Python
// harness must consume the REAL Go buildDigest output, not a re-port.
// This test replays the probe scenario into a live Orchestrator, emits
// digests the way WaitingState does, and asserts the push artifacts carry
// every property the probe's grading depends on. It also dumps the digests
// to the probe directory as context_arm_digest.txt.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

type c3Task struct {
	ID        string   `json:"id"`
	Status    string   `json:"status"`
	Goal      string   `json:"goal"`
	AgentType string   `json:"agent_type"`
	Parents   []string `json:"parents"`
	Summary   string   `json:"summary,omitempty"`
	Error     string   `json:"error,omitempty"`
}

type c3Scenario struct {
	Goal         string   `json:"goal"`
	Tasks        []c3Task `json:"tasks"`
	ReceiverTask string   `json:"receiver_task"`
	ReceiverID   string   `json:"receiver_task_id"`
}

func loadC3Scenario(t *testing.T) c3Scenario {
	t.Helper()
	raw, err := os.ReadFile("spikes/004-c3-probe/c3_scenario.json")
	if err != nil {
		t.Fatalf("read scenario: %v", err)
	}
	var sc c3Scenario
	if err := json.Unmarshal(raw, &sc); err != nil {
		t.Fatalf("parse scenario: %v", err)
	}
	return sc
}

func buildC3Orchestrator(t *testing.T, sc c3Scenario) (*Orchestrator, *recordingMnemo) {
	t.Helper()
	m := &recordingMnemo{}
	o := NewOrchestrator(context.Background(), &Config{RepoPath: "/tmp/c3"}, m, nil)
	o.goal = sc.Goal
	for _, tt := range sc.Tasks {
		node := &TaskNode{ID: tt.ID, Status: tt.Status, Goal: tt.Goal, AgentType: tt.AgentType, ParentIDs: tt.Parents, Summary: tt.Summary}
		if err := o.dag.AddTask(node); err != nil {
			t.Fatalf("add task %s: %v", tt.ID, err)
		}
	}
	// trip the dep-audit breaker (2 failed dep-audit tasks in scenario)
	cb := NewCircuitBreaker("dep-audit", 2, time.Second)
	cb.RecordFailure()
	cb.RecordFailure()
	o.breakerRegistry = map[string]*CircuitBreaker{"dep-audit": cb}
	return o, m
}

func TestC3FixtureIntegrity(t *testing.T) {
	sc := loadC3Scenario(t)
	o, m := buildC3Orchestrator(t, sc)

	// The receiver's task must exist, be claimed, and depend on the task
	// that found the SQL vulnerability — otherwise the probe's task text
	// is not consistent with the board state any arm sees.
	node, ok := o.dag.nodes[sc.ReceiverID]
	if !ok {
		t.Fatalf("receiver task %s missing from DAG", sc.ReceiverID)
	}
	if node.Status != "claimed" {
		t.Fatalf("receiver task must be claimed, got %s", node.Status)
	}
	foundVulnSource := false
	for _, p := range node.ParentIDs {
		if pn := o.dag.nodes[p]; pn != nil && strings.Contains(pn.Summary, "SQL") {
			foundVulnSource = true
		}
	}
	if !foundVulnSource {
		t.Fatalf("receiver task must depend on a task whose summary contains the SQL finding")
	}

	// Emit digests the way the real pipeline does and verify push artifacts.
	o.emitDigests(context.Background())
	pub := m.callsInCategory("pipeline_digest")
	if len(pub) != 2 {
		t.Fatalf("expected observer+worker digests pushed, got %d", len(pub))
	}
	for _, c := range pub {
		if c.Target != "push" {
			t.Fatalf("digest target must be push, got %q", c.Target)
		}
		if !strings.Contains(c.Content, "ANOMALIES") || !strings.Contains(c.Content, "dep-audit") {
			t.Fatalf("digest must carry the anomalies block (Q10 gap closed)\ngot: %s", c.Content)
		}
	}

	// Dump the observer digest for the Python probe to consume verbatim.
	var observerDigest string
	for _, c := range pub {
		if strings.Contains(c.Key, "observer") {
			observerDigest = c.Content
		}
	}
	if observerDigest == "" {
		t.Fatal("observer digest not found in push artifacts")
	}
	if err := os.WriteFile("spikes/004-c3-probe/context_arm_digest.txt", []byte(observerDigest), 0o644); err != nil {
		t.Fatalf("write digest dump: %v", err)
	}
	t.Logf("observer digest (%d chars):\n%s", len(observerDigest), observerDigest)

	// Also dump the raw firehose equivalent (full matrix incl. anomalies).
	var rawLines []string
	for _, tt := range sc.Tasks {
		line := fmt.Sprintf("task %s: status=%s agent=%s goal=%s", tt.ID, tt.Status, tt.AgentType, tt.Goal)
		if tt.Summary != "" {
			line += " summary=" + tt.Summary
		}
		if tt.Error != "" {
			line += " error=" + tt.Error
		}
		if len(tt.Parents) > 0 {
			line += " parents=" + strings.Join(tt.Parents, ",")
		}
		rawLines = append(rawLines, line)
	}
	rawLines = append(rawLines, "ANOMALIES: dep-audit breaker open (2 failures)")
	rawLines = append(rawLines, "circuit_breakers: dep-audit=open")
	rawLines = append(rawLines, "receiver_task_assignment: "+sc.ReceiverTask)
	raw := strings.Join(rawLines, "\n")
	if err := os.WriteFile("spikes/004-c3-probe/context_arm_raw.txt", []byte(raw), 0o644); err != nil {
		t.Fatalf("write raw dump: %v", err)
	}
}
