package gaap

import (
	"strings"
	"testing"

	vclient "github.com/aj-nt/vassago-sdk/client"
)

// A denied task is never dispatched, so the digest and the failure report are the
// only places a model or an operator sees the refusal. Both must show the
// reframed text; the raw rule text stays on the node as evidence.
func TestDigestPrefersReframedSummary(t *testing.T) {
	o := digestTestOrchestrator(&vclient.NullMnemo{})
	node := o.dag.nodes["T1"]
	node.Status = "failed"
	node.Summary = "policy deny: no rule allows Execute"
	node.SummaryModel = "blocked by your platform's governance policy, not the target's defense: shell actions are not permitted in this run."

	d := o.buildDigest("observer")
	if !strings.Contains(d, "your platform") {
		t.Fatalf("digest does not carry the reframe:\n%s", d)
	}
	if strings.Contains(d, "no rule allows") {
		t.Fatalf("digest leaked the raw policy text to the model:\n%s", d)
	}
}

// Without a reframe the digest must fall back to the raw summary — a worker's
// DONE: line has no SummaryModel and must not disappear.
func TestDigestFallsBackToRawSummary(t *testing.T) {
	o := digestTestOrchestrator(&vclient.NullMnemo{})
	completeTask(o, "T1", "3 findings: HIGH SQL injection")

	d := o.buildDigest("observer")
	if !strings.Contains(d, "3 findings") {
		t.Fatalf("digest dropped a summary that had no reframe:\n%s", d)
	}
}

// An empty reframe must not blank out the summary: displaySummary prefers the
// model text only when there is one.
func TestDisplaySummaryFallsBackWhenModelIsEmpty(t *testing.T) {
	n := &TaskNode{Summary: "policy deny: no rule allows Execute"}
	if got := displaySummary(n); got != n.Summary {
		t.Fatalf("displaySummary = %q, want the raw summary", got)
	}
	n.SummaryModel = "blocked by your platform's governance policy."
	if got := displaySummary(n); got != n.SummaryModel {
		t.Fatalf("displaySummary = %q, want the reframe", got)
	}
	if displaySummary(nil) != "" {
		t.Fatal("displaySummary(nil) should be empty")
	}
}
