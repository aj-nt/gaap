package platform

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDockerEnforcerRunChownsThenRuns(t *testing.T) {
	var calls []string
	e := &DockerEnforcer{
		ChownCommand: "chown", // override so the test doesn't need sudo
		MkdirCommand: "mkdir -p",
		Socket:       "unix:///var/run/gaap.sock",
		run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			calls = append(calls, name+" "+strings.Join(args, " "))
			return []byte("cmd output"), nil
		},
	}
	spec := NewNamespaceSpec(10001, "/var/gaap/agents/a1")
	out, err := e.Run(context.Background(), spec, "alpine", "echo hi")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "cmd output" {
		t.Errorf("Run should return command output, got %q", out)
	}
	if len(calls) != 3 {
		t.Fatalf("expected 3 exec calls (mkdir + chown + docker), got %d: %v", len(calls), calls)
	}
	if !strings.Contains(calls[0], "mkdir -p /var/gaap/agents/a1") {
		t.Errorf("first call should mkdir the workspace: %q", calls[0])
	}
	if !strings.Contains(calls[1], "chown 10001:10001 /var/gaap/agents/a1") {
		t.Errorf("second call should chown workspace to agent uid: %q", calls[1])
	}
	// One-shot run: --host <socket>, run --rm, deny-by-default flags, /bin/sh -c.
	if !strings.Contains(calls[2], "docker --host unix:///var/run/gaap.sock run --rm") {
		t.Errorf("docker call should target the dedicated socket and use run --rm: %q", calls[2])
	}
	for _, want := range []string{"--network none", "--cap-drop ALL", "--read-only", "--user 10001:10001", "-w /workspace", " /bin/sh -c echo hi"} {
		if !strings.Contains(calls[2], want) {
			t.Errorf("docker call missing %q: %q", want, calls[2])
		}
	}
	// One-shot Run must NOT use detached mode.
	if strings.Contains(calls[2], " -d ") {
		t.Errorf("Run must not be detached: %q", calls[2])
	}
}

func TestDockerEnforcerStartIsDetached(t *testing.T) {
	var calls []string
	e := &DockerEnforcer{
		ChownCommand: "chown",
		MkdirCommand: "mkdir -p",
		Socket:       "unix:///var/run/gaap.sock",
		run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			calls = append(calls, name+" "+strings.Join(args, " "))
			return []byte("abcdef123456"), nil
		},
	}
	handle, err := e.Start(context.Background(), NewNamespaceSpec(10001, "/w"), "alpine", "sleep 100")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if handle != "abcdef123456" {
		t.Errorf("Start should return the container handle, got %q", handle)
	}
	if len(calls) != 3 {
		t.Fatalf("expected 3 calls (mkdir + chown + docker), got %d: %v", len(calls), calls)
	}
	if !strings.Contains(calls[2], "docker --host unix:///var/run/gaap.sock run -d --name gaap-sleep-100-") {
		t.Errorf("Start should be detached and uniquely named (gaap-sleep-100-<rand>): %q", calls[2])
	}
	if !strings.Contains(calls[2], "--network none") {
		t.Errorf("Start must still be deny-by-default: %q", calls[2])
	}
}

func TestDockerEnforcerLogsAndStop(t *testing.T) {
	var calls []string
	e := &DockerEnforcer{
		Socket: "unix:///var/run/gaap.sock",
		run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			calls = append(calls, name+" "+strings.Join(args, " "))
			return []byte("some logs"), nil
		},
	}
	if _, err := e.Logs(context.Background(), "gaap-sleep-100"); err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if err := e.Stop(context.Background(), "gaap-sleep-100"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(calls))
	}
	if calls[0] != "docker --host unix:///var/run/gaap.sock logs gaap-sleep-100" {
		t.Errorf("Logs call wrong: %q", calls[0])
	}
	if calls[1] != "docker --host unix:///var/run/gaap.sock rm -f gaap-sleep-100" {
		t.Errorf("Stop call wrong: %q", calls[1])
	}
}

func TestDockerEnforcerEnsureLaunchesWhenDown(t *testing.T) {
	var launchedName string
	var launchedArgs []string
	daemonLaunched := false
	e := &DockerEnforcer{
		Socket:        "unix:///var/run/gaap.sock",
		DataRoot:      "/var/lib/gaap",
		ExecRoot:      "/var/run/gaap-exec",
		Pidfile:       "/var/run/gaap.pid",
		DaemonCommand: "dockerd",
		run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			if name == "docker" && args[len(args)-1] == "info" {
				if !daemonLaunched {
					return nil, context.Canceled // down until launch
				}
				return []byte("{}"), nil // up after launch
			}
			return nil, nil
		},
		launch: func(name string, args []string) error {
			launchedName = name
			launchedArgs = append([]string(nil), args...)
			daemonLaunched = true
			return nil
		},
	}
	if err := e.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if launchedName != "dockerd" {
		t.Fatalf("expected dockerd launch, got %q", launchedName)
	}
	want := "--data-root /var/lib/gaap --exec-root /var/run/gaap-exec --pidfile /var/run/gaap.pid --host unix:///var/run/gaap.sock --bridge=none --iptables=false --ip-forward=false"
	if strings.Join(launchedArgs, " ") != want {
		t.Errorf("dedicated-daemon launch args wrong:\n got: %v\nwant: %v", launchedArgs, want)
	}
}

func TestDockerEnforcerEnsureIdempotentWhenUp(t *testing.T) {
	launches := 0
	e := &DockerEnforcer{
		Socket: "unix:///var/run/gaap.sock",
		run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			if name == "docker" && args[len(args)-1] == "info" {
				return []byte("{}"), nil // daemon up
			}
			return nil, nil
		},
		launch: func(name string, args []string) error {
			launches++
			return nil
		},
	}
	if err := e.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if launches != 0 {
		t.Errorf("Ensure must not launch when the daemon is already up (launched %d times)", launches)
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

func TestDockerEnforcerStagePreparesWorkspaceThenWrites(t *testing.T) {
	var calls []string
	e := &DockerEnforcer{
		ChownCommand: "chown",
		MkdirCommand: "mkdir -p",
		CpCommand:    "cp",
		run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			calls = append(calls, name+" "+strings.Join(args, " "))
			if name == "cp" {
				// Actually perform the copy so content flow is verifiable.
				src, dst := args[len(args)-2], args[len(args)-1]
				b, err := os.ReadFile(src)
				if err != nil {
					return nil, err
				}
				return nil, os.WriteFile(dst, b, 0o644)
			}
			return nil, nil
		},
	}
	spec := NewNamespaceSpec(10001, t.TempDir())
	if err := e.Stage(context.Background(), spec, "code.py", "print(1)\n"); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	// mkdir + chown dir + cp + chown file = at least 3 calls.
	if len(calls) < 3 {
		t.Fatalf("expected mkdir/chown prep + cp + file chown, got %v", calls)
	}
	// The file must exist on disk with the staged content.
	b, err := os.ReadFile(filepath.Join(spec.Workspace, "code.py"))
	if err != nil {
		t.Fatalf("staged file not written: %v", err)
	}
	if string(b) != "print(1)\n" {
		t.Errorf("staged content = %q", string(b))
	}
	// A cp call must target the workspace file path.
	foundCp := false
	for _, c := range calls {
		if strings.HasPrefix(c, "cp ") && strings.HasSuffix(c, "/code.py") {
			foundCp = true
		}
	}
	if !foundCp {
		t.Errorf("expected cp into workspace code.py, got %v", calls)
	}
}

func TestDockerEnforcerStageRejectsTraversal(t *testing.T) {
	e := &DockerEnforcer{ChownCommand: "chown", MkdirCommand: "mkdir -p",
		run: func(ctx context.Context, name string, args ...string) ([]byte, error) { return nil, nil }}
	spec := NewNamespaceSpec(10001, t.TempDir())
	for _, bad := range []string{"../evil.py", "/etc/passwd", "a/../../evil.py"} {
		if err := e.Stage(context.Background(), spec, bad, "x"); err == nil {
			t.Errorf("Stage(%q) should be rejected", bad)
		}
	}
}

func TestDockerEnforcerWaitReturnsExitCode(t *testing.T) {
	var waitArg string
	e := &DockerEnforcer{
		Socket: "unix:///var/run/gaap.sock",
		run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			if name == "docker" && len(args) >= 2 && args[len(args)-2] == "wait" {
				waitArg = args[len(args)-1]
				return []byte("0\n"), nil
			}
			return nil, nil
		},
	}
	out, err := e.Wait(context.Background(), "gaap-sleep-100")
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if out != "0" {
		t.Errorf("Wait should return trimmed exit code, got %q", out)
	}
	if waitArg != "gaap-sleep-100" {
		t.Errorf("Wait should target the handle, got %q", waitArg)
	}
}

func TestDockerEnforcerStatusParsesInspect(t *testing.T) {
	e := &DockerEnforcer{
		Socket: "unix:///var/run/gaap.sock",
		run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			if name == "docker" && len(args) >= 1 && args[len(args)-1] == "gaap-sleep-100" {
				return []byte("running 0\n"), nil
			}
			return nil, nil
		},
	}
	status, exitCode, err := e.Status(context.Background(), "gaap-sleep-100")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status != "running" {
		t.Errorf("status = %q, want running", status)
	}
	if exitCode != 0 {
		t.Errorf("exitCode = %d, want 0", exitCode)
	}
}

func TestDockerEnforcerStatusExited(t *testing.T) {
	e := &DockerEnforcer{
		Socket: "unix:///var/run/gaap.sock",
		run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return []byte("exited 3\n"), nil
		},
	}
	status, exitCode, err := e.Status(context.Background(), "gaap-sleep-100")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status != "exited" {
		t.Errorf("status = %q, want exited", status)
	}
	if exitCode != 3 {
		t.Errorf("exitCode = %d, want 3", exitCode)
	}
}
