//go:build darwin

package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aj-nt/gaap/internal/worker"
	"github.com/aj-nt/gaap/platform"
)

// W3b's field check, on a host that cannot contain anything: the REAL DockerEnforcer
// against the real machine. On macOS there is no dedicated dockerd, so Ensure fails
// and the run must be refused — not quietly executed on the host.
//
// Build-tagged darwin because that is where this proves something: a Linux CI host
// might genuinely have docker, and a test whose outcome depends on the runner's
// docker install is a test that fails for the wrong reason.
func TestRealEnforcerFailsClosedOnMacOS(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	r := containedRunner(ctx, t.TempDir(), &platform.DockerEnforcer{})
	if _, ok := r.(worker.RefuseRunner); !ok {
		t.Fatalf("runner = %T (%s), want RefuseRunner — coercion is impossible here", r, r.Describe())
	}

	out := r.Run(ctx, "echo this should never run", t.TempDir())
	if out.ExitCode == 0 {
		t.Fatalf("the command ran with no containment: %+v", out)
	}
	if !strings.Contains(out.Stderr, "BLOCKED") {
		t.Fatalf("refusal does not announce itself: %q", out.Stderr)
	}
	if !strings.Contains(out.Stderr, "allow-host-exec") {
		t.Fatalf("refusal does not say how to opt in: %q", out.Stderr)
	}
}
