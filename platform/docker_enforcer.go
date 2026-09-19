package platform

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// DockerEnforcer drives the docker CLI to spawn and control the agent's
// container. It runs on the Linux host, as the sovereign's account (which
// holds the docker group + passwordless sudo for the chown step).
type DockerEnforcer struct {
	// ChownCommand is the chown command prefix, default "sudo chown" (the
	// sovereign's account owns the agent's host dirs via sudo). Override to
	// "chown" when running as root. Test override: "chown".
	ChownCommand string
	// run executes a command. Injectable for unit tests; defaults to
	// exec.CommandContext.
	run func(ctx context.Context, name string, args ...string) ([]byte, error)
}

func (e *DockerEnforcer) exec(ctx context.Context, name string, args ...string) ([]byte, error) {
	if e.run != nil {
		return e.run(ctx, name, args...)
	}
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

func (e *DockerEnforcer) chownCommand() string {
	if e.ChownCommand != "" {
		return e.ChownCommand
	}
	return "sudo chown"
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

// Spawn chowns the workspace to the agent uid, then docker-runs the image with
// the deny-by-default namespace. The command string runs through the
// container's shell (/bin/sh -c), so shell syntax (redirection, pipes) works.
// Returns the container ID.
func (e *DockerEnforcer) Spawn(ctx context.Context, spec *NamespaceSpec, image, command string) (string, error) {
	if err := e.chown(ctx, spec.UID, spec.Workspace); err != nil {
		return "", err
	}
	args := append(spec.DockerRunArgs(), "--name", "gaap-agent", image, "/bin/sh", "-c", command)
	out, err := e.exec(ctx, "docker", args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// Stop stops and removes the container.
func (e *DockerEnforcer) Stop(ctx context.Context, id string) error {
	_, err := e.exec(ctx, "docker", "rm", "-f", id)
	return err
}

// Grant applies a path grant. Docker cannot add a mount to a running container,
// so this is a placeholder until Milestone 3's re-spawn flow. For now it only
// chowns the granted path (the part that is valid standalone).
func (e *DockerEnforcer) Grant(ctx context.Context, id string, g PathGrant) error {
	return e.chown(ctx, g.UID, g.Path)
}
