package platform

import (
	"strings"
	"testing"
)

func TestNamespaceSpecSafeFlagsPresent(t *testing.T) {
	s := NewNamespaceSpec(10001, "/var/gaap/agents/a1")
	args := s.DockerRunArgs()
	joined := " " + strings.Join(args, " ") + " "
	for _, want := range []string{
		" --network none ",
		" --cap-drop ALL ",
		" --read-only ",
		" --user 10001:10001 ",
		" -w /workspace ",
		" -v /var/gaap/agents/a1:/workspace:rw ",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("DockerRunArgs missing %q in %v", want, args)
		}
	}
}

func TestNamespaceSpecNeverPrivilegedOrSocket(t *testing.T) {
	s := NewNamespaceSpec(10001, "/w")
	joined := " " + strings.Join(s.DockerRunArgs(), " ") + " "
	for _, forbidden := range []string{
		"--privileged",
		"docker.sock",
		"--network host",
		"--pid host",
		"--cap-add",
	} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("DockerRunArgs must never contain %q", forbidden)
		}
	}
}

// DockerRunArgs hardcoded exactly one -v, so a container could reach its workspace and
// nothing else. ExtraMounts is the seam for the one file it does need — the broker
// socket — and it renders read-only when asked.
func TestExtraMountsRenderReadOnly(t *testing.T) {
	spec := NewNamespaceSpec(1001, "/srv/ws")
	spec.ExtraMounts = []Mount{
		{Source: "/run/gaapd/govern.sock", Target: "/run/gaapd/govern.sock", ReadOnly: true},
	}
	args := strings.Join(spec.DockerRunArgs(), " ")

	if !strings.Contains(args, "-v /run/gaapd/govern.sock:/run/gaapd/govern.sock:ro") {
		t.Fatalf("socket mount missing or writable: %s", args)
	}
	if !strings.Contains(args, "-v /srv/ws:/workspace:rw") {
		t.Fatalf("workspace mount changed: %s", args)
	}
}

func TestExtraMountsKeepTheDenyByDefaultFlags(t *testing.T) {
	spec := NewNamespaceSpec(1001, "/srv/ws")
	spec.ExtraMounts = []Mount{{Source: "/s", Target: "/t"}}
	args := strings.Join(spec.DockerRunArgs(), " ")

	for _, want := range []string{"--network none", "--cap-drop ALL", "--read-only", "--user 1001:1001"} {
		if !strings.Contains(args, want) {
			t.Fatalf("an extra mount dropped %q: %s", want, args)
		}
	}
	// ReadOnly unset means rw — explicit, because silently defaulting either way turns
	// a caller's mistake into a boundary.
	if !strings.Contains(args, "-v /s:/t:rw") {
		t.Fatalf("default mount mode is not rw: %s", args)
	}
}

func TestNoExtraMountsChangesNothing(t *testing.T) {
	base := strings.Join(NewNamespaceSpec(501, "/w").DockerRunArgs(), " ")
	if strings.Count(base, "-v") != 1 {
		t.Fatalf("a spec with no extra mounts rendered %d -v flags: %s", strings.Count(base, "-v"), base)
	}
}
