package platform

import (
	"context"
	"strings"
	"testing"
)

func TestDockerEnforcerRunChownsThenRuns(t *testing.T) {
	var calls []string
	e := &DockerEnforcer{
		ChownCommand: "chown", // override so the test doesn't need sudo
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
	if len(calls) != 2 {
		t.Fatalf("expected 2 exec calls (chown + docker), got %d: %v", len(calls), calls)
	}
	if !strings.Contains(calls[0], "chown 10001:10001 /var/gaap/agents/a1") {
		t.Errorf("first call should chown workspace to agent uid: %q", calls[0])
	}
	// One-shot run: --host <socket>, run --rm, deny-by-default flags, /bin/sh -c.
	if !strings.Contains(calls[1], "docker --host unix:///var/run/gaap.sock run --rm") {
		t.Errorf("docker call should target the dedicated socket and use run --rm: %q", calls[1])
	}
	for _, want := range []string{"--network none", "--cap-drop ALL", "--read-only", "--user 10001:10001", " /bin/sh -c echo hi"} {
		if !strings.Contains(calls[1], want) {
			t.Errorf("docker call missing %q: %q", want, calls[1])
		}
	}
	// One-shot Run must NOT use detached mode.
	if strings.Contains(calls[1], " -d ") {
		t.Errorf("Run must not be detached: %q", calls[1])
	}
}

func TestDockerEnforcerStartIsDetached(t *testing.T) {
	var calls []string
	e := &DockerEnforcer{
		ChownCommand: "chown",
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
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(calls))
	}
	if !strings.Contains(calls[1], "docker --host unix:///var/run/gaap.sock run -d --name gaap-sleep-100") {
		t.Errorf("Start should be detached and named: %q", calls[1])
	}
	if !strings.Contains(calls[1], "--network none") {
		t.Errorf("Start must still be deny-by-default: %q", calls[1])
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
	e := &DockerEnforcer{
		Socket:        "unix:///var/run/gaap.sock",
		DataRoot:      "/var/lib/gaap",
		ExecRoot:      "/var/run/gaap-exec",
		Pidfile:       "/var/run/gaap.pid",
		DaemonCommand: "dockerd",
		run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			// docker --host <socket> info → daemon down.
			if name == "docker" && args[len(args)-1] == "info" {
				return nil, context.Canceled
			}
			return nil, nil
		},
		launch: func(name string, args []string) error {
			launchedName = name
			launchedArgs = append([]string(nil), args...)
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
