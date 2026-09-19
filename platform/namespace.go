package platform

import "fmt"

// NamespaceSpec is the deny-by-default container configuration, verified by
// spike 006. It renders to the docker run flags that produce blast-radius
// containment: no network, no capabilities, read-only root, a non-root uid,
// and a single narrow workspace mount.
type NamespaceSpec struct {
	UID       int    // per-agent uid (not shared nobody — spike 006)
	GID       int    // per-agent gid
	Workspace string // host path mounted rw into /workspace
	ReadOnly  bool   // container root fs read-only (default true)
}

// NewNamespaceSpec returns the deny-by-default spec for one agent.
func NewNamespaceSpec(uid int, workspace string) *NamespaceSpec {
	return &NamespaceSpec{UID: uid, GID: uid, Workspace: workspace, ReadOnly: true}
}

// DockerRunArgs returns the docker run arguments (image and command are
// appended by the caller). Safe flags only, in docker's expected order.
func (s *NamespaceSpec) DockerRunArgs() []string {
	args := []string{"run", "--rm"}
	args = append(args, "--network", "none")
	args = append(args, "--cap-drop", "ALL")
	args = append(args, "--user", fmt.Sprintf("%d:%d", s.UID, s.GID))
	if s.ReadOnly {
		args = append(args, "--read-only")
	}
	args = append(args, "-v", fmt.Sprintf("%s:/workspace:rw", s.Workspace))
	return args
}
