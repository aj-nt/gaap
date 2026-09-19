package platform

import "testing"

func TestPathGrantCarriesChown(t *testing.T) {
	g := PathGrant{Path: "/data/foo", Mode: "rw", UID: 10001}
	if g.ChownTarget() != "10001:10001" {
		t.Fatalf("ChownTarget = %q, want 10001:10001", g.ChownTarget())
	}
	if g.ChownTarget() == "" {
		t.Fatal("path grant must specify its chown target")
	}
}
