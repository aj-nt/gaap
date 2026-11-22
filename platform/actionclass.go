package platform

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
