package platform

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
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

func (c MemoryCapability) known() bool {
	switch c {
	case MemRead, MemWrite, MemResolve, MemTombstone, MemInjectable:
		return true
	}
	return false
}

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

type memoryGrantFile struct {
	Version int           `json:"version"`
	Grants  []MemoryGrant `json:"grants"`
}

// LoadMemoryGrants compiles a grant file to deny-first rules and returns the policy
// revision it was compiled from.
//
// The revision is the sha256 of the file's bytes, not its `version` field: a format
// version is "1" for every grant file ever written, so recording it would put the
// same string on every row and prove nothing.
func LoadMemoryGrants(path string) ([]Rule, string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	var file memoryGrantFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, "", fmt.Errorf("memory grants %s: %w", path, err)
	}
	sum := sha256.Sum256(raw)
	rev := hex.EncodeToString(sum[:])
	rules, err := CompileMemoryGrants(file.Grants, rev)
	if err != nil {
		return nil, rev, err
	}
	return rules, rev, nil
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
	rules := make([]Rule, 0, len(grants)+len(memoryTools))
	for _, g := range grants {
		if !g.Capability.known() {
			return nil, fmt.Errorf("memory grant %q: unknown capability %q (want read, write, resolve, tombstone or injectable)", g.ID, g.Capability)
		}
		if strings.TrimSpace(g.Scope) == "" {
			return nil, fmt.Errorf("memory grant %q: scope is required; an unscoped grant is a grant to everyone", g.ID)
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
			Tool:     "mem." + string(g.Capability),
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
	for _, tool := range memoryTools {
		rules = append(rules, Rule{
			ID:       "mem-deny-" + strings.TrimPrefix(tool, "mem."),
			Tool:     tool,
			Decision: Decision{Action: ActionDeny, Reason: "no memory grant opens " + tool},
		})
	}
	return rules, nil
}

// memoryTools is the store's whole capability surface. Adding a capability here
// closes it by default, which is the point of writing the denies from this list.
var memoryTools = []string{
	"mem.read", "mem.write", "mem.resolve", "mem.tombstone", "mem.injectable",
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
