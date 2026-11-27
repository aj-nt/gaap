package platform

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// MemoryCapability is one thing a grant permits against the memory store.
//
// Capabilities rather than tool names, because the store is reached over a socket
// by a shim whose tool vocabulary belongs to the agent framework. A grant that named
// tools would have to be rewritten every time an agent framework renamed one, and
// the rewrite is exactly where a boundary goes missing.
type MemoryCapability string

const (
	MemRead      MemoryCapability = "read"
	MemWrite     MemoryCapability = "write"
	MemResolve   MemoryCapability = "resolve"
	MemTombstone MemoryCapability = "tombstone"
	// MemInjectable marks a grant whose entries may be placed in the always-injected
	// block. It is separate from read/write because "may be read on request" and
	// "is placed in the prompt unasked" are different exposures.
	MemInjectable MemoryCapability = "injectable"
)

// allCapabilities is the whole capability vocabulary, and the single source for both
// `known()` and the deny list. Stated once because the two lists have to agree: a
// capability declared in the const block and missing from the deny list is a tool the
// engine still refuses by fallthrough, but with an empty RuleID, and a refusal that
// cannot name the rule that produced it is the M6.1 defect. Deriving both from this
// slice is what makes the next capability closed by default.
var allCapabilities = []MemoryCapability{MemRead, MemWrite, MemResolve, MemTombstone, MemInjectable}

func (c MemoryCapability) known() bool { return slices.Contains(allCapabilities, c) }

// MemoryTool is the wire name of a capability. Exported because the broker spells it too,
// and a broker op that is not a declared capability is a tool nothing has a rule for.
func MemoryTool(c MemoryCapability) string { return "mem." + string(c) }

// MemoryGrant is one line of a grant file.
type MemoryGrant struct {
	// ID is optional; when empty it is derived from capability and scope. It is never
	// empty in the compiled rule, because a rule with no id produces a ledger entry
	// that cannot say which rule refused — the M6.1 defect.
	ID         string           `json:"id"`
	Capability MemoryCapability `json:"capability"`
	Scope      string           `json:"scope"` // an agent id, or "*" for all
	MaxTTL     int64            `json:"max_ttl_seconds"`
}

// CompileMemoryGrants turns grants into rules: the allows a grant file permits, then
// one explicit deny per memory tool behind them.
//
// The denies are stated rather than inherited. The engine's fallthrough already
// denies an unmatched call, but its RuleID is empty, and a refusal that cannot name
// the rule that produced it is the M6.1 defect. They also have to come LAST: this
// engine is first-match-wins, so a deny emitted first would shadow every grant and
// the file would be a very elaborate way of writing "no".
func CompileMemoryGrants(grants []MemoryGrant, revision string) ([]Rule, error) {
	rules := make([]Rule, 0, len(grants)+len(allCapabilities))
	for _, g := range grants {
		if !g.Capability.known() {
			return nil, fmt.Errorf("memory grant %q: unknown capability %q (want read, write, resolve, tombstone or injectable)", g.ID, g.Capability)
		}
		if strings.TrimSpace(g.Scope) == "" {
			return nil, fmt.Errorf("memory grant %q: scope is required; an unscoped grant is a grant to everyone", g.ID)
		}
		// "*" is documented as "all agents" but the predicate engine compares identity
		// exactly, so a wildcard grant compiles and then matches nobody — every capability it
		// was meant to open stays shut, silently. Refusing here turns a silent deny into a
		// startup error. Whether a wildcard should exist at all (one grant covering agents
		// that do not exist yet) is a separate decision about authority.
		if strings.TrimSpace(g.Scope) == "*" {
			return nil, fmt.Errorf("memory grant %q: wildcard scope \"*\" is not honoured (identity is matched exactly); name the agents explicitly", g.ID)
		}
		ttl := time.Duration(g.MaxTTL) * time.Second
		if g.Capability == MemWrite {
			// A write with no lifetime is a permanent entry, and permanence is the
			// thing a memory store must not grant by omission.
			if ttl <= 0 {
				return nil, fmt.Errorf("memory grant %q: write grants require max_ttl_seconds > 0", g.ID)
			}
			if ttl > ArtifactCeiling {
				return nil, fmt.Errorf("memory grant %q: max_ttl_seconds %s exceeds the %s ceiling", g.ID, ttl, ArtifactCeiling)
			}
		}
		id := g.ID
		if id == "" {
			id = "mem-cap-" + string(g.Capability) + "-" + slugScope(g.Scope)
		}
		r := Rule{
			ID:       id,
			Tool:     MemoryTool(g.Capability),
			Decision: Decision{Action: ActionAllow, Reason: "memory grant " + id},
			// Identity is a stable predicate: the caller cannot rename its way past it.
			Predicates: []Predicate{{Kind: PredIdentity, Value: g.Scope}},
		}
		if ttl > 0 {
			r.Artifact = &Artifact{
				Type:      ArtifactType("memory_entry"),
				Lifetime:  ttl,
				Scope:     g.Scope,
				Ceiling:   ArtifactCeiling,
				Locatable: true,
			}
		}
		rules = append(rules, r)
	}
	for _, c := range allCapabilities {
		tool := MemoryTool(c)
		rules = append(rules, Rule{
			ID:       "mem-deny-" + strings.TrimPrefix(tool, "mem."),
			Tool:     tool,
			Decision: Decision{Action: ActionDeny, Reason: "no memory grant opens " + tool},
		})
	}
	return rules, nil
}

func slugScope(s string) string {
	if s == "*" {
		return "all"
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

// GrantsEngine builds an engine from a grant file. The signature matters: a caller
// that can construct the engine itself can add a rule the grants never permitted,
// so the compiled ruleset is what gets handed out.
func GrantsEngine(rules []Rule) *PolicyEngine {
	e := NewPolicyEngine()
	for _, r := range rules {
		e.AddRule(r)
	}
	return e
}
