package platform

// Reversibility classifies whether an effect can be undone, in three classes
// (from the Cordis paper's "revertible effects" distinction, arXiv 2608.25512):
//
//   - Pure: no side effect at all; nothing to undo (reads).
//   - Reversible: the pre-state determines the inverse; undo is snapshot-and-restore.
//   - Compensable: undo needs the effect's OUTPUT; undo is "good enough", not clean.
//   - Irreversible: no inverse exists (network send, filesystem write, subprocess).
//
// Honesty rule (the Cordis lesson, one level up): never record "reversible" on
// trust. A class that claims an inverse the runtime does not actually hold is
// the same unchecked-inverse error the paper makes, reproduced in our audit
// trail. Reversible and Compensable are therefore RESERVED — classify never
// assigns them until a real snapshot/compensation mechanism exists to back the
// claim. Today the live classes are Pure (reads) and Irreversible (everything
// that mutates or executes).
type Reversibility string

const (
	Pure         Reversibility = "pure"
	Reversible   Reversibility = "reversible"
	Compensable  Reversibility = "compensable"
	Irreversible Reversibility = "irreversible"
)

// classify returns the reversibility class for a tool, known at decision time
// from the tool name. Conservative by default: anything not recognized as a
// read is irreversible. Reads are the only class whose guarantee the runtime
// holds today (a read provably performs no mutation).
func classify(tool string) Reversibility {
	switch tool {
	case "ReadFile", "FileRead", "FileSearch":
		return Pure
	default:
		return Irreversible
	}
}
