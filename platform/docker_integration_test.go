//go:build integration

package platform

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDockerEnforcerIntegration is the real-thing proof: spawn an agent
// container under the deny-by-default namespace, write to the workspace, and
// verify the write reached the host (which only works if the enforcer's chown
// fixed host-side ownership — spike 006's actionable finding).
//
// This test requires docker and sudo chown on a Linux host, so it is excluded
// from CI by the integration build tag and run manually:
//
//	go test -tags=integration ./platform/ -run TestDockerEnforcerIntegration -v
func TestDockerEnforcerIntegration(t *testing.T) {
	e := &DockerEnforcer{ChownCommand: "sudo chown"}
	ctx := context.Background()
	ws := t.TempDir() // host dir owned by the test user

	// The enforcer chowns the workspace to the agent uid (correct — spike 006).
	// Restore ownership on the way out so t.TempDir() cleanup can delete the
	// agent-created files (the test user can't delete files owned by uid 10001).
	defer e.chown(ctx, os.Getuid(), ws)

	spec := NewNamespaceSpec(10001, ws)
	id, err := e.Spawn(ctx, spec, "alpine:latest",
		"echo hi > /workspace/probe.txt")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer e.Stop(ctx, id)

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
