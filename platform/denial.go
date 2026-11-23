package platform

import (
	"crypto/rand"
	"encoding/hex"
	"sort"
	"strings"
)

// NewCorrelationID returns a short random handle. 8 bytes is enough to join a
// message to a ledger row and short enough to quote in a prompt.
func NewCorrelationID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail in practice; a deterministic fallback would
		// silently produce duplicate ids, which is worse than saying so.
		panic("platform: no entropy for a correlation id: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// ReframeDenial renders a refusal for the agent that was refused.
//
// Three things it deliberately does NOT do: quote the rule (its text encodes the
// threshold), quote the raw reason (same), or name the target. Those stay in the
// ledger where the operator can read them. It does say who decided, because an
// agent that mistakes a governance block for a WAF will spend its remaining
// budget trying to get around its own platform.
func ReframeDenial(d DenialClass) string {
	var b strings.Builder
	b.WriteString("blocked by your platform's governance policy, not the target's defense: ")

	if d.ActionClass.Known() {
		b.WriteString(string(d.ActionClass))
		b.WriteString(" actions are not permitted in this run")
	} else if d.Forbidden != "" {
		b.WriteString(d.Forbidden)
		b.WriteString(" is not permitted in this run")
	} else {
		b.WriteString("this call is not permitted in this run")
	}
	b.WriteString(".")

	if len(d.Permitted) == 0 {
		b.WriteString(" No other tool is allowed for this agent type, so retrying with a different name will not help.")
	} else {
		permitted := append([]string(nil), d.Permitted...)
		sort.Strings(permitted)
		b.WriteString(" Permitted alternatives: ")
		b.WriteString(strings.Join(permitted, ", "))
		b.WriteString(".")
	}

	if d.CorrelationID != "" {
		b.WriteString(" Reference ")
		b.WriteString(d.CorrelationID)
		b.WriteString(" for the operator's record.")
	}
	return b.String()
}
