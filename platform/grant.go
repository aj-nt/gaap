package platform

import (
	"errors"
	"fmt"
	"time"
)

// PathGrant is a scoped, time-boxed host-access grant for a filesystem path.
// Spike 006: a :rw bind-mount under --user <uid> writes or fails purely on host
// directory ownership, so a PathGrant carries the chown target the enforcer
// must apply before the mount is live.
type PathGrant struct {
	Path string // host path
	Mode string // "ro" or "rw"
	UID  int    // agent uid that must own the host path
	// ExpiresAt bounds the grant. A grant is standing authority to write to a
	// host path, created by a single transient decision, so there is no default:
	// the zero value is refused rather than read as "forever".
	ExpiresAt time.Time
}

// Validate reports whether the grant may still be applied at now. It refuses a
// grant with no path and one with no expiry, because a grant that never ends is
// permanent authority minted by one decision.
func (g PathGrant) Validate(now time.Time) error {
	if g.Path == "" {
		return errors.New("path grant names no path")
	}
	if g.ExpiresAt.IsZero() {
		return fmt.Errorf("path grant for %s has no expiry: a grant without a deadline is permanent authority", g.Path)
	}
	if !now.Before(g.ExpiresAt) {
		return fmt.Errorf("path grant for %s expired at %s", g.Path, g.ExpiresAt.UTC().Format(time.RFC3339))
	}
	return nil
}

// ChownTarget returns the "uid:gid" the host path must be chowned to.
func (g PathGrant) ChownTarget() string {
	return fmt.Sprintf("%d:%d", g.UID, g.UID)
}
