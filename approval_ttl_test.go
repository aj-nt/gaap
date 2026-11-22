package gaap

import (
	"testing"
	"time"
)

// The approval task is where a human answer is written. If it expires before the
// run stops waiting, a late-but-valid answer has nowhere to land and the run
// reports that nobody replied. The task lifetime must therefore always exceed the
// deadline the run itself honors.
func TestApprovalTaskOutlivesItsDeadline(t *testing.T) {
	for _, deadline := range []time.Duration{
		0,
		1 * time.Second,
		60 * time.Second,
		DefaultApprovalTimeoutSec * time.Second,
		3600 * time.Second,
	} {
		ttl := time.Duration(ApprovalTaskTTL(deadline)) * time.Second
		if ttl <= deadline {
			t.Fatalf("ApprovalTaskTTL(%s) = %s, want strictly greater than the deadline the run waits for",
				deadline, ttl)
		}
	}
}

// The CLI default must not be able to drift from the task lifetime: the whole
// point is that one number feeds both.
func TestDefaultApprovalTimeoutProducesLongerTask(t *testing.T) {
	deadline := time.Duration(DefaultApprovalTimeoutSec) * time.Second
	ttl := time.Duration(ApprovalTaskTTL(deadline)) * time.Second
	if ttl <= deadline {
		t.Fatalf("default %s deadline yields a %s task; the run would wait past the task's expiry",
			deadline, ttl)
	}
}
