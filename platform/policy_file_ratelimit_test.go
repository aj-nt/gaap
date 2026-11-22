package platform

import (
	"strings"
	"testing"
)

// A policy author who writes rate_limit gets a rule the kernel cannot honor:
// nothing counts anything, dispatch's switch has no case for it, and the default
// arm turns it into a deny. Accepting the file would mean the operator believes
// they configured a throttle and actually configured a block.
func TestRateLimitRejectedAtLoad(t *testing.T) {
	const p = `{"version":1,"rules":[
	  {"id":"throttle-exec","tool":"Execute","action":"rate_limit","reason":"too fast"}
	]}`
	_, err := ParsePolicy(strings.NewReader(p))
	if err == nil {
		t.Fatal("ParsePolicy accepted rate_limit; dispatch would silently turn it into a deny")
	}
	if !strings.Contains(err.Error(), "throttle-exec") {
		t.Fatalf("error does not name the rule: %v", err)
	}
	if !strings.Contains(err.Error(), "rate_limit") {
		t.Fatalf("error does not name the action: %v", err)
	}
	if !strings.Contains(err.Error(), "not enforced") {
		t.Fatalf("error does not say the action is unenforced: %v", err)
	}
}

// The rejection must not come at the cost of the actions that do work.
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
