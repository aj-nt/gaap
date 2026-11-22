package platform

import (
	"strings"
	"testing"
)

func TestReframeAttributesToPlatform(t *testing.T) {
	got := ReframeDenial(DenialClass{
		Rule: "no-destructive-shell", ActionClass: ClassShell, Forbidden: "Execute",
		Permitted: []string{"ReadFile"}, CorrelationID: "abc123",
	})
	if !strings.Contains(got, "your platform") {
		t.Fatalf("reframe does not attribute authorship: %q", got)
	}
	if !strings.Contains(got, "not the target") {
		t.Fatalf("reframe does not distinguish the platform from the target: %q", got)
	}
	if !strings.Contains(got, "shell") {
		t.Fatalf("reframe does not name the forbidden class: %q", got)
	}
	if !strings.Contains(got, "abc123") {
		t.Fatalf("reframe does not carry the correlation id: %q", got)
	}
}

// The rule id and the operator's reason both encode the threshold. An agent that
// reads them learns exactly which string to avoid.
func TestReframeWithholdsRuleID(t *testing.T) {
	const rule = "no-destructive-shell"
	got := ReframeDenial(DenialClass{
		Rule: rule, ActionClass: ClassShell, Forbidden: "Execute",
		Permitted: []string{"ReadFile"}, CorrelationID: "abc123",
	})
	if strings.Contains(got, rule) {
		t.Fatalf("reframe leaked the rule id (%q), which encodes the threshold: %q", rule, got)
	}
	if strings.Contains(got, "no rule allows") {
		t.Fatalf("reframe carried the raw reason: %q", got)
	}
}

func TestReframeListsPermitted(t *testing.T) {
	got := ReframeDenial(DenialClass{
		ActionClass: ClassShell, Permitted: []string{"WriteFile", "ReadFile"},
	})
	ri := strings.Index(got, "ReadFile")
	wi := strings.Index(got, "WriteFile")
	if ri < 0 || wi < 0 {
		t.Fatalf("reframe does not list the permitted tools: %q", got)
	}
	if ri > wi {
		t.Fatalf("permitted list is not sorted, so the text is unstable: %q", got)
	}
}

func TestReframeWithoutPermittedSaysSo(t *testing.T) {
	got := ReframeDenial(DenialClass{ActionClass: ClassShell, Forbidden: "Execute"})
	if !strings.Contains(got, "retrying with a different name will not help") {
		t.Fatalf("reframe does not close off the reframe path when nothing is permitted: %q", got)
	}
}

func TestReframeFallsBackToToolName(t *testing.T) {
	got := ReframeDenial(DenialClass{Forbidden: "Deploy"})
	if !strings.Contains(got, "Deploy") {
		t.Fatalf("reframe with no class does not name the tool: %q", got)
	}
}

func TestCorrelationIDIsRandomHex(t *testing.T) {
	a, b := NewCorrelationID(), NewCorrelationID()
	if len(a) != 16 {
		t.Fatalf("correlation id %q is not 16 hex chars", a)
	}
	if a == b {
		t.Fatalf("correlation ids repeat: %q", a)
	}
}

func TestActionClassKnown(t *testing.T) {
	for _, c := range []ActionClass{ClassShell, ClassFileWrite, ClassNetwork, ClassMemory, ClassSelfExtension} {
		if !c.Known() {
			t.Errorf("%q should be known", c)
		}
	}
	if ActionClass("").Known() {
		t.Error("empty class must not count as known")
	}
	if ActionClass("shells").Known() {
		t.Error("an undeclared class must not count as known")
	}
}
