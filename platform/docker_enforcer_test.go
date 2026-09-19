package platform

import (
	"context"
	"strings"
	"testing"
)

func TestDockerEnforcerChownsBeforeSpawn(t *testing.T) {
	var calls []string
	e := &DockerEnforcer{
		ChownCommand: "chown", // override so the test doesn't need sudo
		run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			calls = append(calls, name+" "+strings.Join(args, " "))
			return []byte("container-1"), nil
		},
	}
	spec := NewNamespaceSpec(10001, "/var/gaap/agents/a1")
	if _, err := e.Spawn(context.Background(), spec, "alpine", "true"); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("expected 2 exec calls (chown + docker), got %d: %v", len(calls), calls)
	}
	if !strings.Contains(calls[0], "chown 10001:10001 /var/gaap/agents/a1") {
		t.Errorf("first call should chown workspace to agent uid: %q", calls[0])
	}
	if !strings.Contains(calls[1], "docker run --rm --network none") {
		t.Errorf("second call should be docker run with deny-by-default: %q", calls[1])
	}
}

func TestDockerEnforcerGrantChowns(t *testing.T) {
	var calls []string
	e := &DockerEnforcer{
		ChownCommand: "chown",
		run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			calls = append(calls, name+" "+strings.Join(args, " "))
			return nil, nil
		},
	}
	if err := e.Grant(context.Background(), "c1", PathGrant{Path: "/data/foo", Mode: "rw", UID: 10001}); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if len(calls) != 1 || !strings.Contains(calls[0], "chown 10001:10001 /data/foo") {
		t.Errorf("Grant should chown the path: %v", calls)
	}
}
