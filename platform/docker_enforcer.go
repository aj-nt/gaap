package platform

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// DockerEnforcer drives Gaap's own dedicated dockerd to run the agent's
// arbitrary code inside a deny-by-default container. It runs on a Linux host,
// as the sovereign's account (which holds root via sudo for both the daemon
// launch and the chown step).
//
// The dedicated daemon is the independence primitive: Gaap owns its own
// dockerd (data-root, exec-root, pidfile, and unix socket all under Gaap's
// control), so coercion does not depend on the host's docker being installed,
// healthy, or even present. The host docker is left untouched.
type DockerEnforcer struct {
	// Socket is the docker socket to talk to, default unix:///var/run/gaap.sock.
	Socket string
	// DataRoot is the dedicated daemon's image/container store, default
	// /var/lib/gaap.
	DataRoot string
	// ExecRoot is the dedicated daemon's exec root, default /var/run/gaap-exec.
	ExecRoot string
	// Pidfile is the dedicated daemon's pidfile, default /var/run/gaap.pid.
	Pidfile string
	// ChownCommand is the chown command prefix, default "sudo chown". Override
	// to "chown" when running as root. Test override: "chown".
	ChownCommand string
	// MkdirCommand is the mkdir command prefix, default "sudo mkdir -p".
	// Override to "mkdir -p" when running as root.
	MkdirCommand string
	// DaemonCommand is the dockerd binary, default "dockerd".
	DaemonCommand string

	// ensureOnce guards daemon launch so Ensure is idempotent and safe under
	// concurrent first-use.
	ensureOnce sync.Once
	ensureErr  error

	// run executes a command. Injectable for unit tests; defaults to
	// exec.CommandContext.
	run func(ctx context.Context, name string, args ...string) ([]byte, error)

	// launch starts the dedicated daemon. Injectable for unit tests; defaults
	// to exec.Command(...).Start() + detached Wait.
	launch func(name string, args []string) error
}

func (e *DockerEnforcer) socket() string {
	if e.Socket != "" {
		return e.Socket
	}
	return "unix:///var/run/gaap.sock"
}

func (e *DockerEnforcer) dataRoot() string {
	if e.DataRoot != "" {
		return e.DataRoot
	}
	return "/var/lib/gaap"
}

func (e *DockerEnforcer) execRoot() string {
	if e.ExecRoot != "" {
		return e.ExecRoot
	}
	return "/var/run/gaap-exec"
}

func (e *DockerEnforcer) pidfile() string {
	if e.Pidfile != "" {
		return e.Pidfile
	}
	return "/var/run/gaap.pid"
}

func (e *DockerEnforcer) chownCommand() string {
	if e.ChownCommand != "" {
		return e.ChownCommand
	}
	return "sudo chown"
}

func (e *DockerEnforcer) mkdirCommand() string {
	if e.MkdirCommand != "" {
		return e.MkdirCommand
	}
	return "sudo mkdir -p"
}

func (e *DockerEnforcer) daemonCommand() string {
	if e.DaemonCommand != "" {
		return e.DaemonCommand
	}
	return "sudo dockerd"
}

func (e *DockerEnforcer) exec(ctx context.Context, name string, args ...string) ([]byte, error) {
	if e.run != nil {
		return e.run(ctx, name, args...)
	}
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// docker runs a docker command against the dedicated daemon's socket.
func (e *DockerEnforcer) docker(ctx context.Context, args ...string) ([]byte, error) {
	full := append([]string{"--host", e.socket()}, args...)
	return e.exec(ctx, "docker", full...)
}

// chown runs the chown command against the given host path for the agent uid.
func (e *DockerEnforcer) chown(ctx context.Context, uid int, path string) error {
	parts := strings.Fields(e.chownCommand())
	if len(parts) == 0 {
		return fmt.Errorf("empty chown command")
	}
	name := parts[0]
	args := append(parts[1:], fmt.Sprintf("%d:%d", uid, uid), path)
	_, err := e.exec(ctx, name, args...)
	return err
}

// mkdirAll ensures the workspace directory exists before chown, so the
// bind-mount target and its ownership are both ready before the container
// starts.
func (e *DockerEnforcer) mkdirAll(ctx context.Context, path string) error {
	parts := strings.Fields(e.mkdirCommand())
	if len(parts) == 0 {
		return fmt.Errorf("empty mkdir command")
	}
	name := parts[0]
	args := append(parts[1:], path)
	_, err := e.exec(ctx, name, args...)
	return err
}

// prepWorkspace makes the host workspace exist and owned by the agent uid —
// the host-side prerequisite for a :rw bind-mount under --user <uid>.
func (e *DockerEnforcer) prepWorkspace(ctx context.Context, spec *NamespaceSpec) error {
	if err := e.mkdirAll(ctx, spec.Workspace); err != nil {
		return err
	}
	return e.chown(ctx, spec.UID, spec.Workspace)
}

// Ensure launches the dedicated dockerd if it is not already up, then waits
// for it to answer on its socket (bounded). Idempotent and safe under
// concurrent first-use. The wait is what makes "self-managed" real: launching
// the daemon without confirming it is ready hands the caller a race.
func (e *DockerEnforcer) Ensure(ctx context.Context) error {
	e.ensureOnce.Do(func() {
		if e.daemonUp(ctx) {
			return
		}
		if err := e.launchDaemon(ctx); err != nil {
			e.ensureErr = err
			return
		}
		e.ensureErr = e.waitReady(ctx)
	})
	return e.ensureErr
}

// waitReady polls the daemon socket until it answers, or the context expires.
func (e *DockerEnforcer) waitReady(ctx context.Context) error {
	const (
		attempts = 30
		delay    = 500 * time.Millisecond
	)
	for i := 0; i < attempts; i++ {
		if e.daemonUp(ctx) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
	return fmt.Errorf("dedicated dockerd did not become ready after %d attempts", attempts)
}

// daemonUp reports whether the dedicated daemon answers on its socket.
func (e *DockerEnforcer) daemonUp(ctx context.Context) bool {
	_, err := e.docker(ctx, "info")
	return err == nil
}

// daemonArgs returns the dedicated dockerd launch flags: own data-root,
// exec-root, pidfile, and socket; no bridge/iptables/ip-forward (the daemon
// does not wire host networking — the per-container --network none is the real
// gate).
func (e *DockerEnforcer) daemonArgs() []string {
	return []string{
		"--data-root", e.dataRoot(),
		"--exec-root", e.execRoot(),
		"--pidfile", e.pidfile(),
		"--host", e.socket(),
		"--bridge=none", "--iptables=false", "--ip-forward=false",
	}
}

// launchDaemon starts the dedicated dockerd in the background. The daemon
// command is a prefix (default "sudo dockerd") split like chown, so the daemon
// runs as root via the sovereign's passwordless sudo.
func (e *DockerEnforcer) launchDaemon(ctx context.Context) error {
	parts := strings.Fields(e.daemonCommand())
	if len(parts) == 0 {
		return fmt.Errorf("empty daemon command")
	}
	name := parts[0]
	args := append(parts[1:], e.daemonArgs()...)
	if e.launch != nil {
		return e.launch(name, args)
	}
	cmd := exec.Command(name, args...)
	// Capture stderr so a silent startup failure is diagnosable.
	if log, err := os.Create("/tmp/gaap-dockerd.log"); err == nil {
		cmd.Stderr = log
		cmd.Stdout = log
		defer func() { _ = log.Close() }()
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("launch dedicated dockerd: %w", err)
	}
	// Detach: the daemon keeps running after this call returns.
	go func() { _ = cmd.Wait() }()
	return nil
}

// Run executes a one-shot command in a deny-by-default container and returns
// its combined output. The container is removed when the command exits.
func (e *DockerEnforcer) Run(ctx context.Context, spec *NamespaceSpec, image, command string) (string, error) {
	if err := e.prepWorkspace(ctx, spec); err != nil {
		return "", err
	}
	args := append([]string{"run", "--rm"}, spec.DockerRunArgs()...)
	args = append(args, image, "/bin/sh", "-c", command)
	out, err := e.docker(ctx, args...)
	if err != nil {
		return string(out), err
	}
	return string(out), nil
}

// Start launches a detached (long-running) command in a deny-by-default
// container and returns a handle (the container name) used by Logs/Stop.
func (e *DockerEnforcer) Start(ctx context.Context, spec *NamespaceSpec, image, command string) (string, error) {
	if err := e.prepWorkspace(ctx, spec); err != nil {
		return "", err
	}
	handle := "gaap-" + shortID(command)
	args := append([]string{"run", "-d", "--name", handle}, spec.DockerRunArgs()...)
	args = append(args, image, "/bin/sh", "-c", command)
	out, err := e.docker(ctx, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// Logs returns the accumulated output of a detached container.
func (e *DockerEnforcer) Logs(ctx context.Context, handle string) (string, error) {
	out, err := e.docker(ctx, "logs", handle)
	return string(out), err
}

// Stage writes a file into the agent's workspace mount and chowns it to the
// agent uid, so a code file can be executed inside the container. The
// workspace must exist (mkdir + chown dir first), then the file is written
// host-side and its ownership fixed so the container's non-root user can read
// and overwrite it.
func (e *DockerEnforcer) Stage(ctx context.Context, spec *NamespaceSpec, filename, content string) error {
	if err := e.prepWorkspace(ctx, spec); err != nil {
		return err
	}
	// Reject any filename that escapes the workspace (path traversal).
	if strings.Contains(filename, "..") || strings.HasPrefix(filename, "/") {
		return fmt.Errorf("stage filename %q must be a relative path inside the workspace", filename)
	}
	path := filepath.Join(spec.Workspace, filename)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("stage %s: %w", filename, err)
	}
	if err := e.chown(ctx, spec.UID, path); err != nil {
		return fmt.Errorf("stage chown %s: %w", filename, err)
	}
	return nil
}

// Wait blocks until a detached container exits and returns its exit code
// (trimmed). The container is NOT removed, so Logs still works afterward.
func (e *DockerEnforcer) Wait(ctx context.Context, handle string) (string, error) {
	out, err := e.docker(ctx, "wait", handle)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// Stop terminates and removes a detached container.
func (e *DockerEnforcer) Stop(ctx context.Context, handle string) error {
	_, err := e.docker(ctx, "rm", "-f", handle)
	return err
}

// Grant applies a path grant. Docker cannot add a mount to a running container,
// so this is a placeholder until the re-spawn flow. For now it only chowns the
// granted path (the part that is valid standalone).
func (e *DockerEnforcer) Grant(ctx context.Context, handle string, g PathGrant) error {
	return e.chown(ctx, g.UID, g.Path)
}

// shortID derives a stable, sanitized container name from a command string.
func shortID(command string) string {
	const max = 12
	var b strings.Builder
	for _, r := range command {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
		if b.Len() >= max {
			break
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		s = "run"
	}
	return s
}
