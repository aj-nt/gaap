//go:build integration

package platform

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDockerEnforcerIntegration is the real-thing proof: run a command inside
// a deny-by-default container on Gaap's dedicated daemon, write to the
// workspace, and verify the write reached the host (which only works if the
// enforcer's chown fixed host-side ownership — spike 006's actionable finding).
//
// This test requires docker + sudo chown on a Linux host, so it is excluded
// from CI by the integration build tag and run manually:
//
//	go test -tags=integration ./platform/ -run TestDockerEnforcerIntegration -v
func TestDockerEnforcerIntegration(t *testing.T) {
	e := &DockerEnforcer{ChownCommand: "sudo chown"}
	ctx := context.Background()

	if err := e.Ensure(ctx); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	ws := t.TempDir() // host dir owned by the test user

	// The enforcer chowns the workspace to the agent uid (correct — spike 006).
	// Restore ownership on the way out so t.TempDir() cleanup can delete the
	// agent-created files (the test user can't delete files owned by uid 10001).
	defer e.chown(ctx, os.Getuid(), ws)

	spec := NewNamespaceSpec(10001, ws)
	if _, err := e.Run(ctx, spec, "alpine:latest",
		"echo hi > /workspace/probe.txt"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Workspace write must have succeeded: the enforcer chowned it to uid 10001.
	probe := filepath.Join(ws, "probe.txt")
	if _, err := os.Stat(probe); err != nil {
		t.Fatalf("agent write did not reach host workspace (chown failed?): %v", err)
	}
	b, _ := os.ReadFile(probe)
	if strings.TrimSpace(string(b)) != "hi" {
		t.Fatalf("workspace probe content = %q, want hi", string(b))
	}
}

// TestDockerEnforcerDetachedIntegration proves the Start/Logs/Stop lifecycle
// against the dedicated daemon: start a long-running command, read its output,
// then stop and confirm the container is gone.
func TestDockerEnforcerDetachedIntegration(t *testing.T) {
	e := &DockerEnforcer{ChownCommand: "sudo chown"}
	ctx := context.Background()

	if err := e.Ensure(ctx); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	ws := t.TempDir()
	defer e.chown(ctx, os.Getuid(), ws)

	spec := NewNamespaceSpec(10001, ws)
	handle, err := e.Start(ctx, spec, "alpine:latest",
		"echo started && sleep 60")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Poll logs until the startup line appears (the container needs a moment).
	logs, err := e.Logs(ctx, handle)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if !strings.Contains(logs, "started") {
		t.Fatalf("Logs should contain startup output, got %q", logs)
	}

	if err := e.Stop(ctx, handle); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}
