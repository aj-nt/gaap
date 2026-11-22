package platform

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The raw reason is written for the operator and encodes the threshold; the
// reframe is written for the agent. Both must survive, and they must not be the
// same string.
func TestDenyRecordsRawReasonAndReframe(t *testing.T) {
	g := NewGovernor()
	g.Policy.AddRule(Rule{ID: "no-shell", Tool: "Execute", Decision: Deny("no shell here"), ActionClass: ClassShell})
	g.Policy.AddRule(Rule{ID: "read", Tool: "ReadFile", Decision: Allow, ActionClass: ClassFileWrite})

	d := g.Decide("agent-1", "Execute", map[string]any{"command": "rm -rf /"})
	if d.Action != ActionDeny {
		t.Fatalf("action = %q, want deny", d.Action)
	}
	if d.Denial == nil {
		t.Fatal("deny decision carries no structured Denial")
	}
	if d.Denial.CorrelationID == "" {
		t.Fatal("denial has no correlation id, so the text shown cannot be looked up")
	}
	if d.Denial.ActionClass != ClassShell {
		t.Fatalf("denial class = %q, want shell", d.Denial.ActionClass)
	}
	if len(d.Denial.Permitted) != 1 || d.Denial.Permitted[0] != "ReadFile" {
		t.Fatalf("permitted = %v, want [ReadFile]", d.Denial.Permitted)
	}

	entries := g.Ledger.Entries()
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	e := entries[0]
	if e.Reason != "no shell here" {
		t.Fatalf("ledger reason = %q, want the raw rule reason", e.Reason)
	}
	if e.DenialClass != "shell" {
		t.Fatalf("ledger denial_class = %q, want shell", e.DenialClass)
	}
	if e.CorrelationID != d.Denial.CorrelationID {
		t.Fatalf("ledger correlation id %q != the one shown to the agent %q", e.CorrelationID, d.Denial.CorrelationID)
	}
	if strings.Contains(e.Reason, "your platform") {
		t.Fatal("the ledger reason was replaced with reframed text; the record must keep what the rule said")
	}
	if strings.Contains(ReframeDenial(*d.Denial), "no shell here") {
		t.Fatal("the reframe leaked the raw reason, which encodes the threshold")
	}
}

// An allow has nothing to refuse, so the denial fields must stay empty rather
// than carrying a zero-valued class that reads as "shell".
func TestAllowCarriesNoDenial(t *testing.T) {
	g := NewGovernor()
	g.Policy.AddRule(Rule{ID: "read", Tool: "ReadFile", Decision: Allow})
	d := g.Decide("agent-1", "ReadFile", map[string]any{"path": "/etc/hosts"})
	if d.Action != ActionAllow {
		t.Fatalf("action = %q, want allow", d.Action)
	}
	if d.Denial != nil {
		t.Fatalf("allow decision carries a denial: %+v", d.Denial)
	}
	e := g.Ledger.Entries()[0]
	if e.DenialClass != "" || e.CorrelationID != "" {
		t.Fatalf("allow entry has denial fields set: class=%q correlation=%q", e.DenialClass, e.CorrelationID)
	}
}

// A sustained attempt is denied exactly like a single one — the count makes it
// visible in the record, it does not change the verdict. A burst downgraded to
// "ask a human" would be a prober's reward.
func TestBurstIsRecordedWithoutChangingTheVerdict(t *testing.T) {
	dir := t.TempDir()
	g := NewGovernor()
	g.Policy.AddRule(Rule{ID: "no-shell", Tool: "Execute", Decision: Deny("no shell"), ActionClass: ClassShell})
	denials, err := OpenDenialCounter(filepath.Join(dir, "denials.json"), time.Minute, 3)
	if err != nil {
		t.Fatalf("open counter: %v", err)
	}
	g.Denials = denials

	for i := 1; i <= 3; i++ {
		d := g.Decide("agent-1", "Execute", map[string]any{"command": "id"})
		if d.Action != ActionDeny {
			t.Fatalf("attempt %d: action = %q, want deny", i, d.Action)
		}
		if i < 3 && d.Burst != 0 {
			t.Fatalf("attempt %d reported a burst early: %d", i, d.Burst)
		}
		if i == 3 && d.Burst != 3 {
			t.Fatalf("third denial burst = %d, want 3", d.Burst)
		}
	}

	kinds := map[string]int{}
	for _, e := range g.Ledger.Entries() {
		kinds[string(e.Kind)]++
	}
	if kinds[KindDenialBurst] != 1 {
		t.Fatalf("burst entries = %d, want 1 (kinds: %v)", kinds[KindDenialBurst], kinds)
	}
	if kinds[KindDecision] != 3 {
		t.Fatalf("decision entries = %d, want 3", kinds[KindDecision])
	}
}

// A different class is a different bucket: three refusals of shell say nothing
// about a file write.
func TestBurstIsPerClass(t *testing.T) {
	g := NewGovernor()
	g.Policy.AddRule(Rule{ID: "no-shell", Tool: "Execute", Decision: Deny("no shell"), ActionClass: ClassShell})
	g.Policy.AddRule(Rule{ID: "no-write", Tool: "WriteFile", Decision: Deny("no write"), ActionClass: ClassFileWrite})
	denials, err := OpenDenialCounter(filepath.Join(t.TempDir(), "denials.json"), time.Minute, 3)
	if err != nil {
		t.Fatalf("open counter: %v", err)
	}
	g.Denials = denials

	for i := 0; i < 3; i++ {
		g.Decide("agent-1", "Execute", nil)
	}
	d := g.Decide("agent-1", "WriteFile", nil)
	if d.Burst != 0 {
		t.Fatalf("a file_write burst was reported from shell denials: %d", d.Burst)
	}
}

// Counters must not fire on allowed calls: a workload that does its job is not
// probing a boundary.
func TestAllowIsNotCounted(t *testing.T) {
	dir := t.TempDir()
	g := NewGovernor()
	g.Policy.AddRule(Rule{ID: "read", Tool: "ReadFile", Decision: Allow, ActionClass: ClassFileWrite})
	denials, err := OpenDenialCounter(filepath.Join(dir, "denials.json"), time.Minute, 1)
	if err != nil {
		t.Fatalf("open counter: %v", err)
	}
	g.Denials = denials
	for i := 0; i < 5; i++ {
		if d := g.Decide("agent-1", "ReadFile", nil); d.Action != ActionAllow {
			t.Fatalf("allow became %q", d.Action)
		}
	}
	for _, e := range g.Ledger.Entries() {
		if e.Kind == KindDenialBurst {
			t.Fatal("an allowed call produced a burst entry")
		}
	}
}

// The deny-by-default path has no rule, so it has no class — and must still
// produce a correlation id and an honest reframe.
func TestDenyByDefaultStillReframes(t *testing.T) {
	g := NewGovernor()
	d := g.Decide("agent-1", "Execute", nil)
	if d.Action != ActionDeny {
		t.Fatalf("action = %q, want deny", d.Action)
	}
	if d.Denial == nil || d.Denial.CorrelationID == "" {
		t.Fatal("deny-by-default produced no correlation id")
	}
	if d.Denial.ActionClass != "" {
		t.Fatalf("deny-by-default invented a class %q", d.Denial.ActionClass)
	}
	got := ReframeDenial(*d.Denial)
	if !strings.Contains(got, "your platform") {
		t.Fatalf("reframe does not attribute authorship: %q", got)
	}
	if !strings.Contains(got, "retrying with a different name will not help") {
		t.Fatalf("reframe on a closed policy should say retrying will not help: %q", got)
	}
}
