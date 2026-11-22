package platform

import "fmt"

// LintLevel separates "this rule does not mean what it says" from "this rule says
// less than it could". Only the first may fail a build: a linter that turns every
// gap into an error is one operators route around.
type LintLevel string

const (
	LintError   LintLevel = "error"
	LintWarning LintLevel = "warning"
)

// LintFinding is one thing wrong with a policy, addressed to the rule that
// carries it.
type LintFinding struct {
	RuleID  string
	Level   LintLevel
	Message string
}

func (f LintFinding) String() string {
	return fmt.Sprintf("%s: %s: %s", f.Level, f.RuleID, f.Message)
}

// LintPolicy checks the standing orders for the gaps that make the record
// silently weaker than it looks.
//
// The asymmetry is deliberate. An allow rule that permits an irreversible effect
// without saying what it leaves behind is an error: the ledger will record
// "unrecorded" forever and nobody will notice. A rule with no action class is a
// warning: refusals still work, they are just counted in one bucket.
func (e *PolicyEngine) Lint() []LintFinding {
	e.mu.RLock()
	defer e.mu.RUnlock()

	var out []LintFinding
	for _, r := range e.rules {
		if r.Decision.Action == ActionAllow && r.Reversibility == Irreversible && r.Artifact == nil {
			out = append(out, LintFinding{
				RuleID: r.ID,
				Level:  LintError,
				Message: "allows an irreversible effect but declares no artifact, " +
					"so every consequence of this rule is recorded as unrecorded",
			})
		}
		if r.ActionClass == "" {
			out = append(out, LintFinding{
				RuleID: r.ID,
				Level:  LintWarning,
				Message: "declares no action class, so refusals of " + r.Tool +
					" are counted in the unclassified bucket and a renamed retry cannot be joined to them",
			})
		}
	}
	return out
}

// HasErrors reports whether any finding is error-level, which is what a strict
// invocation fails on.
func HasErrors(findings []LintFinding) bool {
	for _, f := range findings {
		if f.Level == LintError {
			return true
		}
	}
	return false
}
