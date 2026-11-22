package worker

import (
	"context"
	"strings"
	"testing"

	"github.com/aj-nt/gaap/platform"
)

// The zero value of the seam decides whether the product contains anything, so it
// has to be the closed one.
func TestNilRunnerRefuses(t *testing.T) {
	e := NewExecutor(&mockChatClient{}, 3, nil)
	if _, ok := e.runner.(RefuseRunner); !ok {
		t.Fatalf("nil runner produced %T, want RefuseRunner", e.runner)
	}
}

func TestRefuseRunnerExecutesNothing(t *testing.T) {
	out := RefuseRunner{Why: "containment unavailable"}.Run(context.Background(), "touch /tmp/should-not-exist", "/tmp")
	if out.ExitCode == 0 {
		t.Fatal("the refuser reported success")
	}
	if !strings.Contains(out.Stderr, "BLOCKED") || !strings.Contains(out.Stderr, "containment unavailable") {
		t.Fatalf("stderr does not explain the refusal: %q", out.Stderr)
	}
}

// A refusal must be attributable: an unattributed one is the defect this layer
// keeps re-learning.
func TestRefuseRunnerWithoutReasonStillSaysSomething(t *testing.T) {
	out := RefuseRunner{}.Run(context.Background(), "ls", "/tmp")
	if !strings.Contains(out.Stderr, "no command runner") {
		t.Fatalf("an unreasoned refusal produced %q", out.Stderr)
	}
}

// The container path must go through the spec, not around it, and must never fall
// back to the host when the enforcer fails.
func TestContainerRunnerUsesTheSpecAndNeverFallsBack(t *testing.T) {
	f := &fakeEnforcer{err: context.DeadlineExceeded}
	r := NewContainerRunner(f, 501, "/tmp/ws", "alpine:3")
	out := r.Run(context.Background(), "echo hi", "/host/path/ignored")

	if f.calls != 1 {
		t.Fatalf("enforcer called %d times, want 1", f.calls)
	}
	if f.spec == nil {
		t.Fatal("enforcer was called with no namespace spec")
	}
	if f.spec.UID != 501 || f.spec.Workspace != "/tmp/ws" || !f.spec.ReadOnly {
		t.Fatalf("spec = %+v, want uid 501, workspace /tmp/ws, read-only", f.spec)
	}
	if f.command != "echo hi" {
		t.Fatalf("command = %q", f.command)
	}
	if out.ExitCode == 0 {
		t.Fatal("a failed container run reported success")
	}
}

func TestContainerRunnerDefaultUIDIsNotSharedRoot(t *testing.T) {
	r := NewContainerRunner(&fakeEnforcer{}, 0, "/tmp/ws", "")
	if r.UID == 0 {
		t.Fatal("uid defaulted to root")
	}
	if r.Image != "alpine:latest" {
		t.Fatalf("image = %q", r.Image)
	}
}

// An unconfigured container runner must refuse rather than panic or execute.
func TestContainerRunnerWithNoEnforcerRefuses(t *testing.T) {
	out := (&ContainerRunner{}).Run(context.Background(), "ls", "/tmp")
	if out.ExitCode != -1 || !strings.Contains(out.Stderr, "BLOCKED") {
		t.Fatalf("output = %+v, want a refusal", out)
	}
}

func TestRunnersDescribeThemselves(t *testing.T) {
	cases := map[string]CommandRunner{
		"host":      HostRunner{},
		"refuse":    RefuseRunner{Why: "x"},
		"container": NewContainerRunner(&fakeEnforcer{}, 501, "/ws", "alpine:3"),
	}
	for want, r := range cases {
		if got := r.Describe(); !strings.Contains(got, want) {
			t.Errorf("Describe() = %q, want it to mention %q", got, want)
		}
	}
}

type fakeEnforcer struct {
	calls   int
	spec    *platform.NamespaceSpec
	command string
	out     string
	err     error
}

func (f *fakeEnforcer) Ensure(context.Context) error { return f.err }

func (f *fakeEnforcer) Run(_ context.Context, spec *platform.NamespaceSpec, image, command string) (string, error) {
	f.calls++
	f.spec = spec
	f.command = command
	return f.out, f.err
}

func (f *fakeEnforcer) Start(context.Context, *platform.NamespaceSpec, string, string) (string, error) {
	return "", nil
}

func (f *fakeEnforcer) Logs(context.Context, string) (string, error) { return "", nil }
func (f *fakeEnforcer) Stage(context.Context, *platform.NamespaceSpec, string, string) error {
	return nil
}
func (f *fakeEnforcer) Wait(context.Context, string) (string, error)            { return "", nil }
func (f *fakeEnforcer) Status(context.Context, string) (string, int, error)     { return "", 0, nil }
func (f *fakeEnforcer) Stop(context.Context, string) error                      { return nil }
func (f *fakeEnforcer) Grant(context.Context, string, platform.PathGrant) error { return nil }
