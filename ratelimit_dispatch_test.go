package gaap

import (
	"strings"
	"testing"

	"github.com/aj-nt/gaap/internal/status"
	"github.com/aj-nt/gaap/platform"
)

// M8.3: a rate_limit rule must reach the dispatcher. M0.1 refused it at load
// precisely because this switch had no case for it, so the rule became a silent
// block; the branch and the acceptance have to land together or the feature is
// configurable but unreachable.
func TestRateLimitDeniesTheTask(t *testing.T) {
	g := policyFor(t, platform.Decision{Action: platform.ActionRateLimit, Reason: "too fast"})
	cfg := governedConfig(g)
	o, td := reviewFixture(t, cfg)

	node := o.dag.nodes["abc12345_scan"]
	if node.Status != status.TaskFailed {
		t.Fatalf("status = %q, want failed", node.Status)
	}
	if len(td.tasks) != 0 {
		t.Fatalf("a rate-limited task reached the daemon: %v", td.tasks)
	}
	if !strings.Contains(node.Summary, "too fast") {
		t.Fatalf("summary = %q, want the rule's reason", node.Summary)
	}
}

// With the flag on, the refusal carries the cooldown the caller needs in order to
// retry — which is the only thing that distinguishes a throttle from a block.
func TestRateLimitCarriesCooldownWhenEnabled(t *testing.T) {
	g := policyFor(t, platform.Decision{Action: platform.ActionRateLimit, Reason: "too fast"})
	g.Denials, _ = platform.OpenDenialCounter(t.TempDir()+"/denials.json", 0, 0)
	cfg := governedConfig(g)
	cfg.RateLimit = true
	o, _ := reviewFixture(t, cfg)

	summary := o.dag.nodes["abc12345_scan"].Summary
	if !strings.Contains(summary, "rate limited") {
		t.Fatalf("summary = %q, want it to say the call was rate limited", summary)
	}
	if !strings.Contains(summary, "retry after") {
		t.Fatalf("summary = %q, want a retry-after", summary)
	}
	if !strings.Contains(summary, "1 refused") {
		t.Fatalf("summary = %q, want the count from the shared counter", summary)
	}
}

// Off by default, and honest about it: the deny happens, but the message does not
// promise a cooldown nothing is enforcing.
func TestRateLimitWithoutFlagPromisesNoCooldown(t *testing.T) {
	g := policyFor(t, platform.Decision{Action: platform.ActionRateLimit, Reason: "too fast"})
	cfg := governedConfig(g)
	cfg.RateLimit = false
	o, _ := reviewFixture(t, cfg)

	summary := o.dag.nodes["abc12345_scan"].Summary
	if strings.Contains(summary, "retry after") {
		t.Fatalf("summary = %q promises a cooldown with enforcement off", summary)
	}
	if !strings.Contains(summary, "too fast") {
		t.Fatalf("summary = %q, want the rule's reason", summary)
	}
}
