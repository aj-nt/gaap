package platform

// Reversibility classifies whether an effect can be undone, in four classes
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
// trail. Reversible and Compensable are therefore RESERVED — never assigned
// until a real snapshot/compensation mechanism exists to back the claim. Today
// the live classes are Pure (reads) and Irreversible (everything that mutates
// or executes).
//
// The kernel does NOT know the governed workload's tool surface, so its default
// classifier is maximally conservative: everything is Irreversible. A workload
// that knows its own read-only tools injects a classifier (Governor.Classify)
// mapping those names to Pure. This keeps the kernel honest (it never guesses
// that an unknown tool is a read) and keeps workload-specific tool names out of
// the public kernel.
type Reversibility string

const (
	Pure         Reversibility = "pure"
	Reversible   Reversibility = "reversible"
	Compensable  Reversibility = "compensable"
	Irreversible Reversibility = "irreversible"
)

// classify is the kernel's default classifier. It marks every tool
// Irreversible: the kernel cannot prove that an arbitrary tool name is a read,
// so it never claims one is. A governed workload replaces this with a
// classifier that knows its own read tools (Governor.Classify).
func classify(tool string) Reversibility {
	return Irreversible
}
