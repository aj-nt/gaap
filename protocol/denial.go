package protocol

// ActionClass is a coarse grouping of what a call is FOR, declared per rule.
//
// It exists because a tool name is authored by the observed party. An agent
// refused at "Execute" can retry as "Shell", "Run" or "Bash" against rules that
// name none of them, and any counter or cooldown keyed on the name resets on the
// first reframe. The class is what the standing orders say the call is for, so a
// reframed retry increments the same counter and the ledger row reads the same
// way.
type ActionClass string

const (
	ClassShell         ActionClass = "shell"
	ClassFileWrite     ActionClass = "file_write"
	ClassNetwork       ActionClass = "network"
	ClassMemory        ActionClass = "memory"
	ClassSelfExtension ActionClass = "self_extension"
)

// Known reports whether c is one of the declared classes. Empty is not known:
// a rule that declares nothing is a gap for the linter, not a silent default.
func (c ActionClass) Known() bool {
	switch c {
	case ClassShell, ClassFileWrite, ClassNetwork, ClassMemory, ClassSelfExtension:
		return true
	}
	return false
}

// String makes the class usable in formatting and ledger output.
func (c ActionClass) String() string { return string(c) }

// DenialClass is the structured refusal carried by a decision.
type DenialClass struct {
	// Rule names the standing order. It is recorded and never rendered: an id
	// like "no-destructive-shell" is a threshold in prose.
	Rule string
	// ActionClass is what the call was for, not what it was called.
	ActionClass ActionClass
	// Forbidden is the tool name that was refused.
	Forbidden string
	// Permitted lists tool names these standing orders do allow. Sorted, so the
	// text is stable across runs and diffable in a report.
	Permitted []string
	// CorrelationID ties what the agent was shown to the row an operator can
	// read. Without it "the governor refused me" is unanswerable.
	CorrelationID string
}
