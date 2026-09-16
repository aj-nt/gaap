package gaap

import (
	"strings"
	"testing"

	vclient "github.com/aj-nt/vassago-sdk/client"
)

// Parity check: the fixture from spike 001 (2 done with findings, 1 blocked)
// must produce a digest carrying substance + attribution within budget.
func TestDigestParityWithSpike(t *testing.T) {
	o := digestTestOrchestrator(&vclient.NullMnemo{})
	completeTask(o, "T1", "3 findings: [HIGH] SQL string concatenation in auth interceptor query builder line 142")
	completeTask(o, "T2", "13 golangci-lint issues: 7 errcheck, 4 staticcheck, 2 revive")

	dObs := o.buildDigest("observer")
	for _, want := range []string{"SQL", "13 golangci-lint", "blocked by", "BLOCKED"} {
		if !strings.Contains(dObs, want) {
			t.Errorf("observer digest missing %q\ngot:\n%s", want, dObs)
		}
	}
	if len(dObs) > 900 {
		t.Errorf("digest %d > budget 900", len(dObs))
	}
	t.Logf("observer digest (%d chars):\n%s", len(dObs), dObs)
}
