package platform

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// Policy file shape. Rules are JSON rather than YAML so the kernel keeps zero
// third-party dependencies; a governor that ships a parser dependency for its
// own rules is a supply-chain decision, not a convenience one.
//
// A rule file is the sovereign's standing orders. It is authored by hand, read
// at startup, and never written by the governor, which is what keeps "who
// decides" separate from "what is decided."
type policyFile struct {
	// Version guards the format. 1 is the only accepted version; absent means 1.
	Version int          `json:"version"`
	Rules   []policyRule `json:"rules"`
}

type policyRule struct {
	// ID names the rule and is recorded on every decision it produces. Required:
	// an unattributable rule is invisible in the audit trail.
	ID     string `json:"id"`
	Tool   string `json:"tool"`
	Action string `json:"action"`
	// Reason accompanies any non-allow decision. Required for non-allow, because
	// the ledger records it and a bare deny teaches the agent nothing.
	Reason string `json:"reason,omitempty"`
	// ArgsContain matches when every listed argument's string form contains its
	// substring. Values are rendered with fmt.Sprint so a number or bool arg can
	// be matched without a type puzzle.
	ArgsContain map[string]string `json:"args_contain,omitempty"`
}

// ParsePolicy reads a rule file and returns the rules in evaluation order.
//
// Validation is strict on purpose. A policy that loads but means something
// different than it reads is worse than one that refuses to load: the operator
// would be governed by rules they did not write.
func ParsePolicy(r io.Reader) ([]Rule, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read policy: %w", err)
	}
	var file policyFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("parse policy: %w", err)
	}
	if file.Version != 0 && file.Version != 1 {
		return nil, fmt.Errorf("unsupported policy version %d (only 1 implemented)", file.Version)
	}

	seen := make(map[string]struct{}, len(file.Rules))
	rules := make([]Rule, 0, len(file.Rules))
	for i, pr := range file.Rules {
		if strings.TrimSpace(pr.ID) == "" {
			return nil, fmt.Errorf("rule %d: missing id", i+1)
		}
		if _, dup := seen[pr.ID]; dup {
			return nil, fmt.Errorf("rule %q: duplicate id (the audit trail would be ambiguous)", pr.ID)
		}
		seen[pr.ID] = struct{}{}
		if strings.TrimSpace(pr.Tool) == "" {
			return nil, fmt.Errorf("rule %q: missing tool", pr.ID)
		}
		action, err := parseAction(pr.Action)
		if err != nil {
			return nil, fmt.Errorf("rule %q: %w", pr.ID, err)
		}
		if action != ActionAllow && strings.TrimSpace(pr.Reason) == "" {
			return nil, fmt.Errorf("rule %q: %s requires a reason", pr.ID, action)
		}
		rules = append(rules, Rule{
			ID:       pr.ID,
			Tool:     pr.Tool,
			Match:    argsContainMatcher(pr.ArgsContain),
			Decision: Decision{Action: action, Reason: pr.Reason},
		})
	}
	return rules, nil
}

// LoadPolicyFile reads a policy file into an engine. A missing or invalid file
// is an error: a governor that silently falls back to no rules is a governor
// that denies everything, which the operator will read as "it is broken."
func LoadPolicyFile(path string) (*PolicyEngine, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open policy %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	rules, err := ParsePolicy(f)
	if err != nil {
		return nil, fmt.Errorf("policy %s: %w", path, err)
	}
	e := NewPolicyEngine()
	for _, rule := range rules {
		e.AddRule(rule)
	}
	return e, nil
}

// parseAction validates an action name against the typed set.
func parseAction(name string) (Action, error) {
	switch Action(name) {
	case ActionAllow, ActionDeny, ActionReview, ActionRateLimit:
		return Action(name), nil
	case "":
		return "", fmt.Errorf("missing action")
	default:
		return "", fmt.Errorf("unknown action %q (want allow, deny, review, or rate_limit)", name)
	}
}

// argsContainMatcher builds the optional predicate for a rule. nil when the rule
// names no arguments, which means the tool match alone decides.
func argsContainMatcher(want map[string]string) func(map[string]any) bool {
	if len(want) == 0 {
		return nil
	}
	return func(args map[string]any) bool {
		for key, substring := range want {
			value, ok := args[key]
			if !ok {
				return false
			}
			if !strings.Contains(fmt.Sprint(value), substring) {
				return false
			}
		}
		return true
	}
}
