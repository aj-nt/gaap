package platform

import (
	"strings"
	"testing"
)

// M8.3 replaced the load-time rejection with an enforcement path. The rule must
// now load, and the action must survive into the Rule — otherwise an operator can
// configure a throttle that never reaches the dispatcher.
func TestRateLimitAcceptedAtLoad(t *testing.T) {
	const p = `{"version":1,"rules":[
	  {"id":"throttle-exec","tool":"Execute","action":"rate_limit","reason":"too fast"}
	]}`
	rules, err := ParsePolicy(strings.NewReader(p))
	if err != nil {
		t.Fatalf("ParsePolicy refused rate_limit: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("rules = %d, want 1", len(rules))
	}
	if rules[0].Decision.Action != ActionRateLimit {
		t.Fatalf("action = %q, want rate_limit", rules[0].Decision.Action)
	}
}

// The rejection's own guard rail, kept: an unknown action must not be mistaken
// for a deliberate one, because the two errors have different fixes.
func TestUnknownActionIsNotRateLimit(t *testing.T) {
	const p = `{"version":1,"rules":[{"id":"x","tool":"Execute","action":"throttle"}]}`
	_, err := ParsePolicy(strings.NewReader(p))
	if err == nil {
		t.Fatal("ParsePolicy accepted an unknown action")
	}
	if !strings.Contains(err.Error(), "unknown action") {
		t.Fatalf("error = %q, want an unknown-action error", err)
	}
	if !strings.Contains(err.Error(), "rate_limit") {
		t.Fatalf("error should list rate_limit among the valid actions: %v", err)
	}
}
func TestEnforcedActionsStillLoad(t *testing.T) {
	const p = `{"version":1,"rules":[
	  {"id":"a","tool":"ReadFile","action":"allow"},
	  {"id":"d","tool":"Execute","action":"deny","reason":"no shell"},
	  {"id":"r","tool":"Deploy","action":"review","reason":"needs a human"}
	]}`
	rules, err := ParsePolicy(strings.NewReader(p))
	if err != nil {
		t.Fatalf("ParsePolicy: %v", err)
	}
	if len(rules) != 3 {
		t.Fatalf("rules = %d, want 3", len(rules))
	}
}

// An unknown action must not be mistaken for the deliberate rejection: the two
// errors have different fixes.
func TestUnknownActionStillSaysUnknown(t *testing.T) {
	const p = `{"version":1,"rules":[{"id":"x","tool":"Execute","action":"throttle"}]}`
	_, err := ParsePolicy(strings.NewReader(p))
	if err == nil {
		t.Fatal("ParsePolicy accepted an unknown action")
	}
	if !strings.Contains(err.Error(), "unknown action") {
		t.Fatalf("error = %q, want an unknown-action error", err)
	}
}
