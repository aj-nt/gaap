package platform

import "context"

// Enforcer applies the sovereign's decisions by spawning and controlling the
// governed agent's container. The real implementation drives docker; a fake
// backs unit tests.
type Enforcer interface {
	// Spawn starts a container under the given namespace. Returns a container
	// identifier used by Stop.
	Spawn(ctx context.Context, spec *NamespaceSpec, image, command string) (string, error)
	// Stop terminates and removes a container.
	Stop(ctx context.Context, id string) error
	// Grant applies a host-access grant (chown + mount for a path). In practice
	// this means re-spawn with the grant present (docker can't add mounts to a
	// running container); the request flow that triggers it is Milestone 3.
	Grant(ctx context.Context, id string, g PathGrant) error
}

// NoopEnforcer is a fake that does nothing. For unit tests and dry runs.
type NoopEnforcer struct{}

func (NoopEnforcer) Spawn(context.Context, *NamespaceSpec, string, string) (string, error) {
	return "noop", nil
}
func (NoopEnforcer) Stop(context.Context, string) error { return nil }
func (NoopEnforcer) Grant(context.Context, string, PathGrant) error { return nil }
