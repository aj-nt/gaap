package platform

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
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
	// Class names what the call is for (shell, file_write, network, memory,
	// self_extension). Optional today; the linter in a later milestone requires
	// it, because a refusal counter keyed on a tool name is defeated by renaming
	// the tool.
	Class string `json:"class,omitempty"`
	// Reversibility is what the standing order says this effect costs. Only
	// pure and irreversible are accepted; the reserved classes are refused
	// rather than recorded on trust.
	Reversibility string `json:"reversibility,omitempty"`
	// Artifact declares the durable thing this rule may create, so the record can
	// say what outlives the run.
	Artifact *artifactSpec `json:"artifact,omitempty"`
	// Predicates narrow the rule to calls satisfying all of them. A deny that
	// rests only on cosmetic predicates is refused at load (M8.5): such a rule is
	// matched by anyone who reads it.
	Predicates []predicateSpec `json:"predicates,omitempty"`
	// MinTaint is the weakest provenance that justifies an allow here.
	MinTaint string `json:"min_taint,omitempty"`
}

// predicateSpec is the file form of a predicate. The class is NOT a field here:
// it is derived from the kind, because an author-declared class that disagrees
// with the kind is a lie with a plausible shape.
type predicateSpec struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

// parsePredicates validates a rule's predicate block.
func parsePredicates(ruleID string, specs []predicateSpec, action Action) ([]Predicate, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	out := make([]Predicate, 0, len(specs))
	for _, s := range specs {
		kind := PredicateKind(strings.TrimSpace(s.Kind))
		if !kind.Known() {
			return nil, fmt.Errorf("rule %q: unknown predicate kind %q (want tool, effect, identity, volume, sequence, user_agent, filename or hash)", ruleID, s.Kind)
		}
		if strings.TrimSpace(s.Value) == "" {
			return nil, fmt.Errorf("rule %q: predicate %q has no value", ruleID, kind)
		}
		out = append(out, Predicate{Kind: kind, Value: s.Value})
	}
	// A deny that only reads what the observed party authors is a deny the
	// observed party can rewrite their way out of. Refused rather than recorded:
	// the rule would look like a control and behave like a suggestion.
	if action == ActionDeny && PredicatesAreCosmeticOnly(out) {
		return nil, fmt.Errorf("rule %q: deny rests only on cosmetic predicates (%s); a rule matched by whoever reads it is not a boundary", ruleID, describePredicates(out))
	}
	return out, nil
}

func describePredicates(ps []Predicate) string {
	parts := make([]string, 0, len(ps))
	for _, p := range ps {
		parts = append(parts, p.String())
	}
	return strings.Join(parts, ", ")
}

// artifactSpec is the file form of an artifact declaration. Lifetime is in
// seconds because a policy file is read by people and by machines, and a
// duration string invites "90d" from one and "2160h" from the other.
type artifactSpec struct {
	Type               string `json:"type"`
	MaxLifetimeSeconds int64  `json:"max_lifetime_seconds,omitempty"`
	Scope              string `json:"scope,omitempty"`
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
	dec := json.NewDecoder(bytes.NewReader(raw))
	// A misspelled key is a rule the operator believes they wrote and did not:
	// "decison": "deny" would load, produce no action, and fail the "missing
	// action" check with a message that names nothing useful. Refusing unknown
	// fields turns it into a load error naming the key.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&file); err != nil {
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
		class := ActionClass(pr.Class)
		if pr.Class != "" && !class.Known() {
			return nil, fmt.Errorf("rule %q: unknown class %q (want shell, file_write, network, memory, or self_extension)", pr.ID, pr.Class)
		}
		rev := Reversibility(strings.TrimSpace(pr.Reversibility))
		switch rev {
		case "", Pure, Irreversible:
		case Reversible, Compensable:
			// The kernel's own rule (reversibility.go): never record a class that
			// claims an inverse the runtime does not hold. A policy file naming one
			// would put that claim on the ledger with no mechanism behind it.
			return nil, fmt.Errorf("rule %q: reversibility %q is not assignable: it is reserved until a snapshot/compensation mechanism exists to back the claim (declare pure or irreversible)", pr.ID, rev)
		default:
			return nil, fmt.Errorf("rule %q: unknown reversibility %q (want pure or irreversible)", pr.ID, rev)
		}
		art, err := parseArtifactSpec(pr.ID, pr.Artifact, rev)
		if err != nil {
			return nil, err
		}
		preds, err := parsePredicates(pr.ID, pr.Predicates, action)
		if err != nil {
			return nil, err
		}
		minTaint := TaintClass(strings.TrimSpace(pr.MinTaint))
		if minTaint != "" && !minTaint.Known() {
			return nil, fmt.Errorf("rule %q: unknown min_taint %q (want external, internal or fact)", pr.ID, pr.MinTaint)
		}
		rules = append(rules, Rule{
			ID:            pr.ID,
			Tool:          pr.Tool,
			Match:         argsContainMatcher(pr.ArgsContain),
			Decision:      Decision{Action: action, Reason: pr.Reason},
			ActionClass:   class,
			Reversibility: rev,
			Artifact:      art,
			Predicates:    preds,
			MinTaint:      minTaint,
		})
	}
	return rules, nil
}

// LoadPolicyFile reads a policy file into an engine. A missing or invalid file
// is an error: a governor that silently falls back to no rules is a governor
// that denies everything, which the operator will read as "it is broken."
func LoadPolicyFile(path string) (*PolicyEngine, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("open policy %s: %w", path, err)
	}

	rules, err := ParsePolicy(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("policy %s: %w", path, err)
	}
	e := NewPolicyEngine()
	for _, rule := range rules {
		e.AddRule(rule)
	}
	// The revision is a content hash, not the file's `version` field: that field
	// is the format version and is the same "1" for every policy ever written.
	// Hashing the bytes means reformatting the file changes the revision, which
	// is intended — the exact instrument in force is what the record should name.
	sum := sha256.Sum256(raw)
	e.setRevision(hex.EncodeToString(sum[:]))
	return e, nil
}

// parseArtifactSpec validates a rule's artifact block and converts it to the
// runtime shape, applying the ceiling in force.
//
// The two refusals here are the ones that would otherwise be recorded as claims:
// an unknown type (a typo creates a bucket nothing else shares), and a pure call
// that creates a durable artifact (pure means no side effect at all, so the pair
// is a lie the ledger would carry).
func parseArtifactSpec(ruleID string, spec *artifactSpec, rev Reversibility) (*Artifact, error) {
	if spec == nil {
		return nil, nil
	}
	typ := ArtifactType(strings.TrimSpace(spec.Type))
	if !typ.Known() {
		return nil, fmt.Errorf("rule %q: unknown artifact type %q", ruleID, spec.Type)
	}
	if rev == Pure {
		return nil, fmt.Errorf("rule %q: declares reversibility pure and artifact type %q; pure means no side effect at all, so the pair cannot both be true", ruleID, typ)
	}
	if spec.MaxLifetimeSeconds < 0 {
		return nil, fmt.Errorf("rule %q: artifact max_lifetime_seconds is negative", ruleID)
	}
	return &Artifact{
		Type:     typ,
		Lifetime: time.Duration(spec.MaxLifetimeSeconds) * time.Second,
		// The ceiling in force is always the kernel's: a per-rule ceiling that
		// defaulted to zero would be a ceiling nothing is ever over.
		Ceiling: ArtifactCeiling,
		Scope:   strings.TrimSpace(spec.Scope),
	}, nil
}

// parseAction validates an action name against the typed set.
func parseAction(name string) (Action, error) {
	switch Action(name) {
	case ActionAllow, ActionDeny, ActionReview:
		return Action(name), nil
	case ActionRateLimit:
		// Accepted, and it is a block: a matching call is refused and the refusal
		// reports the cooldown, with no threshold compared first. M0.1 refused the
		// name at load because nothing counted anything and dispatch had no case,
		// which made the rule a silent block; M8.2 supplied the counter and M8.3 the
		// dispatch case, and reversing that is off the table. What is left is to say
		// plainly what an operator is writing: a refusal, not a queue with a budget.
		return ActionRateLimit, nil
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
