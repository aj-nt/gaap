package platform

// The classes themselves live in the protocol module: a client that only talks to a
// broker needs to name them, and importing the kernel to do so pinned the wire to the
// kernel's release cadence. classify stays here because it is the kernel's judgement,
// not the wire's vocabulary.

// classify is the kernel's default classifier. It marks every tool
// Irreversible: the kernel cannot prove that an arbitrary tool name is a read,
// so it never claims one is. A governed workload replaces this with a
// classifier that knows its own read tools (Governor.Classify).
func classify(tool string) Reversibility {
	return Irreversible
}
