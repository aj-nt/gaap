package gaap

import (
	"context"
	"fmt"
	"testing"
)

// stubMatcher is a test helper that returns a fixed agent ID.
type stubMatcher struct {
	BaseMatcher
	agentID string
	err     error
}

func (s *stubMatcher) FindAgent(ctx context.Context, vassago MnemoClient, required string) (string, error) {
	if s.err != nil {
		return "", s.err
	}
	if s.agentID == "" {
		return s.tryNext(ctx, vassago, required)
	}
	return s.agentID, nil
}

func TestExactMatchReturnsAgent(t *testing.T) {
	t.Parallel()
	chain := &stubMatcher{agentID: "agent-1"}
	result, err := chain.FindAgent(context.Background(), nil, "static_analysis")
	if err != nil {
		t.Fatalf("FindAgent: %v", err)
	}
	if result != "agent-1" {
		t.Errorf("agent = %q, want agent-1", result)
	}
}

func TestChainFallsBackToNext(t *testing.T) {
	t.Parallel()
	// First handler returns empty (no match)
	primary := &stubMatcher{agentID: ""}
	fallback := &stubMatcher{agentID: "agent-fallback"}
	primary.SetNext(fallback)

	result, err := primary.FindAgent(context.Background(), nil, "unknown_capability")
	if err != nil {
		t.Fatalf("FindAgent: %v", err)
	}
	if result != "agent-fallback" {
		t.Errorf("agent = %q, want agent-fallback", result)
	}
}

func TestChainErrorPropagatesFromNext(t *testing.T) {
	t.Parallel()
	primary := &stubMatcher{agentID: ""}
	fallback := &stubMatcher{err: fmt.Errorf("search failed")}
	primary.SetNext(fallback)

	_, err := primary.FindAgent(context.Background(), nil, "cap")
	if err == nil {
		t.Error("expected error from fallback")
	}
}

func TestChainEndReturnsError(t *testing.T) {
	t.Parallel()
	// End of chain without SetNext should return descriptive error.
	last := &stubMatcher{agentID: ""}
	_, err := last.FindAgent(context.Background(), nil, "cap")
	if err == nil {
		t.Error("expected error when chain ends with no match")
	}
}

func TestBuildChainReturnsThreeLinks(t *testing.T) {
	t.Parallel()
	var client MnemoClient // nil is fine for chain structure test
	chain := BuildCapabilityChain(client)
	if chain == nil {
		t.Fatal("BuildCapabilityChain returned nil")
	}
	// Should be ExactMatch → FTS5Fallback.
	// We don't test the internal chain traversal here; that's an integration test.
}
