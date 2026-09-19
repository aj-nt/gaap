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
