package gaap

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/aj-nt/vassago-sdk/client"
)

// MnemoClient is the Vassago daemon client interface used by Gaap.
// Re-exported from the SDK for convenience.
type MnemoClient = client.MnemoClient

// CapabilityMatcher defines a link in the capability matching chain.
// Each handler tries to find an agent; if it can't, it passes to the next.
type CapabilityMatcher interface {
	FindAgent(ctx context.Context, vassago MnemoClient, required string) (string, error)
	SetNext(next CapabilityMatcher)
}

// BaseMatcher provides the SetNext method and tryNext helper for chain links.
type BaseMatcher struct {
	next CapabilityMatcher
}

// SetNext sets the next handler in the chain.
func (b *BaseMatcher) SetNext(next CapabilityMatcher) {
	b.next = next
}

// tryNext passes the request to the next handler. Returns an error if no
// next handler is configured (end of chain with no match).
func (b *BaseMatcher) tryNext(ctx context.Context, vassago MnemoClient, required string) (string, error) {
	if b.next == nil {
		return "", fmt.Errorf("no agent found for capability: %s", required)
	}
	return b.next.FindAgent(ctx, vassago, required)
}

// ExactMatchHandler queries the agent registry memory for an exact capability
// match. Agents register their capabilities as memory entries under the "agent"
// category with structured JSON content.
type ExactMatchHandler struct {
	BaseMatcher
}

// FindAgent attempts an exact match by searching memory for an agent whose
// capability key contains the required capability string.
func (h *ExactMatchHandler) FindAgent(ctx context.Context, vassago MnemoClient, required string) (string, error) {
	// Search for agent capabilities in memory.
	// Memory entries have category="agent" with the agent ID as the key.
	entries, err := vassago.SearchMemories(ctx, required, "memory", 5)
	if err != nil {
		slog.Debug("exact match search failed, falling back", "required", required, "error", err)
		return h.tryNext(ctx, vassago, required)
	}

	if len(entries) == 0 {
		slog.Debug("no exact match, falling back", "required", required)
		return h.tryNext(ctx, vassago, required)
	}

	// For Phase 2, return the first matching agent.
	agentID := entries[0].SourceAgent
	if agentID == "" {
		// If no source_agent, use the key as a fallback identifier.
		agentID = entries[0].Key
	}

	slog.Info("exact capability match", "agent", agentID, "required", required)
	return agentID, nil
}

// FTS5FallbackHandler searches for agents by keyword using FTS5 over
// capability content. This is the second link in the chain, used when
// exact structured matching fails.
type FTS5FallbackHandler struct {
	BaseMatcher
}

// FindAgent attempts a broader keyword-based search for the capability.
func (h *FTS5FallbackHandler) FindAgent(ctx context.Context, vassago MnemoClient, required string) (string, error) {
	// Build an OR-based FTS5 query from the required string.
	// Replace hyphens with spaces because FTS5 parses them as column filters.
	query := buildFTS5Query(required)

	entries, err := vassago.SearchMemories(ctx, query, "memory", 5)
	if err != nil {
		slog.Debug("FTS5 search failed", "query", query, "error", err)
		return h.tryNext(ctx, vassago, required)
	}

	if len(entries) == 0 {
		slog.Debug("FTS5 fallback empty", "query", query)
		return h.tryNext(ctx, vassago, required)
	}

	agentID := entries[0].SourceAgent
	if agentID == "" {
		agentID = entries[0].Key
	}

	slog.Info("FTS5 capability match", "agent", agentID, "query", query)
	return agentID, nil
}

// buildFTS5Query converts a capability string into a safe FTS5 query.
// Hyphens are replaced with spaces, keywords are OR-joined.
func buildFTS5Query(required string) string {
	// Replace column-confusing characters with spaces
	safe := ""
	for _, c := range required {
		if c == '-' || c == ':' || c == '.' {
			c = ' '
		}
		safe += string(c)
	}

	// Simple: just use the sanitized string.
	// OR-separation with * suffix for prefix matching could be added
	// in a later phase if needed.
	return fmt.Sprintf("\"%s\"", safe)
}

// BuildCapabilityChain creates the standard capability matching chain:
// ExactMatch → FTS5Fallback.
func BuildCapabilityChain(vassago MnemoClient) CapabilityMatcher {
	exact := &ExactMatchHandler{}
	fts5 := &FTS5FallbackHandler{}
	exact.SetNext(fts5)
	return exact
}
