package platform

import "fmt"

// PathGrant is a scoped, time-boxed host-access grant for a filesystem path.
// Spike 006: a :rw bind-mount under --user <uid> writes or fails purely on host
// directory ownership, so a PathGrant carries the chown target the enforcer
// must apply before the mount is live.
type PathGrant struct {
	Path string // host path
	Mode string // "ro" or "rw"
	UID  int    // agent uid that must own the host path
}

// ChownTarget returns the "uid:gid" the host path must be chowned to.
func (g PathGrant) ChownTarget() string {
	return fmt.Sprintf("%d:%d", g.UID, g.UID)
}
