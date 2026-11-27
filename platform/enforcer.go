package platform

import (
	"context"
)

// Enforcer applies the sovereign's decisions by running the agent's arbitrary
// code inside a deny-by-default container on Gaap's own dedicated dockerd. The
// real implementation drives the docker CLI; a fake backs unit tests.
type Enforcer interface {
	// Ensure launches Gaap's dedicated dockerd if it is not already running.
	// Idempotent. On a host without the daemon (e.g. non-Linux dev), callers
	// should treat a returned error as "coercion unavailable" and fall back to
	// in-process execution (fail-open, loudly logged).
	Ensure(ctx context.Context) error

	// Run executes a one-shot command in a container and returns its combined
	// output. The container is removed when the command exits.
	Run(ctx context.Context, spec *NamespaceSpec, image, command string) (string, error)

	// Start launches a detached (long-running) command in a container and
	// returns a handle used by Logs/Stop.
	Start(ctx context.Context, spec *NamespaceSpec, image, command string) (string, error)

	// Logs returns the accumulated output of a detached container.
	Logs(ctx context.Context, handle string) (string, error)

	// Stage writes content to a file inside the agent's workspace mount, so a
	// code file can be executed inside the container (ExecuteCode). The file is
	// owned by the agent uid so the container can read and overwrite it.
	Stage(ctx context.Context, spec *NamespaceSpec, filename, content string) error

	// Wait blocks until a detached container exits and returns its exit code
	// (trimmed). The container is NOT removed, so Logs still works afterward.
	Wait(ctx context.Context, handle string) (string, error)

	// Status reports whether a detached container is running or exited and, if
	// exited, its exit code. Non-blocking — used by the long-running lifecycle's
	// "poll" action, which must not wait.
	Status(ctx context.Context, handle string) (status string, exitCode int, err error)

	// Stop terminates and removes a detached container.
	Stop(ctx context.Context, handle string) error
}
