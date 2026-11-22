package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aj-nt/gaap/internal/worker"
	"github.com/aj-nt/gaap/platform"
)

// W3b's decision, as a test: no enforcer, no Execute.
func TestContainmentUnavailableRefuses(t *testing.T) {
	r := containedRunner(context.Background(), "/tmp/repo", &fakeEnforcer{ensureErr: errors.New("no docker socket")})
	if _, ok := r.(worker.RefuseRunner); !ok {
		t.Fatalf("runner = %T, want RefuseRunner", r)
	}
	if !strings.Contains(r.Describe(), "refuse") {
		t.Fatalf("Describe() = %q", r.Describe())
	}
	// The refusal must actually refuse, and carry the underlying reason.
	out := r.Run(context.Background(), "rm -rf /", "/tmp/repo")
	if out.ExitCode == 0 {
		t.Fatal("the run succeeded with no containment")
	}
	if !strings.Contains(out.Stderr, "no docker socket") {
		t.Fatalf("stderr does not carry the reason: %q", out.Stderr)
	}
}

func TestContainmentAvailableUsesTheContainer(t *testing.T) {
	f := &fakeEnforcer{out: "hello"}
	r := containedRunner(context.Background(), "/tmp/repo", f)
	if !strings.Contains(r.Describe(), "container") {
		t.Fatalf("runner = %q, want the container path", r.Describe())
	}
	out := r.Run(context.Background(), "echo hello", "/tmp/repo")
	if f.calls != 1 {
		t.Fatalf("enforcer calls = %d, want 1", f.calls)
	}
	if out.ExitCode != 0 || !strings.Contains(out.Stdout, "hello") {
		t.Fatalf("output = %+v", out)
	}
	if f.spec == nil || f.spec.Workspace != "/tmp/repo" {
		t.Fatalf("workspace mount = %+v, want the repo path", f.spec)
	}
	if f.spec.UID == 0 {
		t.Fatal("the workspace is mounted for uid 0")
	}
}

// The explicit opt-in is the only path to the host, and it is named for what it
// permits.
func TestHostExecIsOptIn(t *testing.T) {
	r := commandRunner(context.Background(), true, "/tmp/repo")
	if _, ok := r.(worker.HostRunner); !ok {
		t.Fatalf("runner = %T, want HostRunner", r)
	}
}

type fakeEnforcer struct {
	ensureErr error
	out       string
	calls     int
	spec      *platform.NamespaceSpec
}

func (f *fakeEnforcer) Ensure(context.Context) error { return f.ensureErr }

func (f *fakeEnforcer) Run(_ context.Context, spec *platform.NamespaceSpec, _, _ string) (string, error) {
	f.calls++
	f.spec = spec
	return f.out, nil
}

func (f *fakeEnforcer) Start(context.Context, *platform.NamespaceSpec, string, string) (string, error) {
	return "", nil
}
func (f *fakeEnforcer) Logs(context.Context, string) (string, error) { return "", nil }
func (f *fakeEnforcer) Stage(context.Context, *platform.NamespaceSpec, string, string) error {
	return nil
}
func (f *fakeEnforcer) Wait(context.Context, string) (string, error)        { return "", nil }
func (f *fakeEnforcer) Status(context.Context, string) (string, int, error) { return "", 0, nil }
func (f *fakeEnforcer) Stop(context.Context, string) error                  { return nil }
func (f *fakeEnforcer) Grant(context.Context, string, platform.PathGrant) error {
	return nil
}
