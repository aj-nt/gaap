package gaap

import (
	"context"

	"github.com/aj-nt/vassago-sdk/client"
)

// testOrchestrator creates a minimal orchestrator for integration tests.
// Uses NullMnemo client — no real daemon needed.
func testOrchestrator() *Orchestrator {
	cfg := &Config{
		RepoPath:        "/tmp/test-repo",
		MaxWaitSec:      30,
		PollIntervalSec: 1,
	}
	return NewOrchestrator(context.Background(), cfg, &client.NullMnemo{}, nil)
}
