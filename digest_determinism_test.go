// This file is part of Vagent.
// See LICENSE-Apache-2.0 for license information.

package gaap

// buildDigest is documented as a pure function of DAG state, but its task order
// came from ranging over o.dag.nodes — a map. The same state therefore rendered
// differently on each run, which makes the digest pushed to observers unstable
// for anything that compares two of them, and makes budget truncation keep a
// random subset of equally-ranked tasks.
//
// These tests pin the order: by status rank, then naturally by task ID, so T2
// comes before T10 rather than after it.

import (
	"strings"
	"testing"
	"time"

	vclient "github.com/aj-nt/vassago-sdk/client"
)

// digestOrderOrchestrator is digestTestOrchestrator (T1 ready, T2 ready, T3
// blocked by T1) widened to eight tasks with three non-closed breakers: enough
// map entries that any residual map-order dependence shows up in a handful of
// runs.
func digestOrderOrchestrator(t *testing.T) *Orchestrator {
	t.Helper()
	o := digestTestOrchestrator(&vclient.NullMnemo{})
	o.digestMaxChars = 100000 // assert the whole order, not the budget edge

	for _, n := range []*TaskNode{
		{ID: "T4", Status: "done", Goal: "g4", AgentType: "a", Summary: "s4"},
		{ID: "T5", Status: "done", Goal: "g5", AgentType: "a", Summary: "s5"},
		{ID: "T6", Status: "blocked", Goal: "g6", AgentType: "a", ParentIDs: []string{"T3"}},
		{ID: "T7", Status: "blocked", Goal: "g7", AgentType: "a", ParentIDs: []string{"T1", "T2"}},
		{ID: "T8", Status: "blocked", Goal: "g8", AgentType: "a", ParentIDs: []string{"T6"}},
		// T10 is the lexicographic trap: as plain strings "T10" < "T6".
		{ID: "T10", Status: "blocked", Goal: "g10", AgentType: "a", ParentIDs: []string{"T2"}},
	} {
		if err := o.dag.AddTask(n); err != nil {
			t.Fatalf("AddTask %s: %v", n.ID, err)
		}
	}
	completeTask(o, "T1", "s1")

	o.breakerRegistry = map[string]*CircuitBreaker{}
	for _, name := range []string{"doc-review", "dep-audit", "style-review"} {
		cb := NewCircuitBreaker(name, 1, time.Minute)
		cb.RecordFailure()
		o.breakerRegistry[name] = cb
	}
	return o
}

func TestCollectDigestTasksOrdersByID(t *testing.T) {
	o := digestOrderOrchestrator(t)

	var ids []string
	for _, task := range collectDigestTasks(o) {
		ids = append(ids, task.id)
	}
	const want = "T1,T2,T3,T4,T5,T6,T7,T8,T10"
	if got := strings.Join(ids, ","); got != want {
		t.Fatalf("collectDigestTasks order:\n got: %s\nwant: %s", got, want)
	}
}

func TestObserverDigestOrdersLinesByStatusThenID(t *testing.T) {
	o := digestOrderOrchestrator(t)
	digest := o.buildDigest("observer")

	// observer selects failed, blocked, and summary-bearing tasks: the five
	// blocked tasks in ID order, then the three with summaries.
	var ids []string
	for _, line := range strings.Split(digest, "\n") {
		if !strings.HasPrefix(line, "  [") {
			continue
		}
		if f := strings.Fields(line); len(f) > 1 {
			ids = append(ids, f[1])
		}
	}
	const want = "T3,T6,T7,T8,T10,T1,T4,T5"
	if got := strings.Join(ids, ","); got != want {
		t.Fatalf("observer digest line order:\n got: %s\nwant: %s\ndigest:\n%s", got, want, digest)
	}
}

func TestBuildDigestIsStableAcrossRuns(t *testing.T) {
	o := digestOrderOrchestrator(t)

	for _, audience := range []string{"observer", "worker", "orchestrator"} {
		first := o.buildDigest(audience)
		for i := 0; i < 50; i++ {
			if got := o.buildDigest(audience); got != first {
				t.Fatalf("%s digest differed between runs (iteration %d)\nfirst:\n%s\nlater:\n%s",
					audience, i, first, got)
			}
		}
	}
}

func TestBreakerAnomaliesOrderIsStable(t *testing.T) {
	o := digestOrderOrchestrator(t)

	first := breakerAnomalies(o)
	for i := 0; i < 50; i++ {
		if got := breakerAnomalies(o); got != first {
			t.Fatalf("anomalies line differed between runs (iteration %d)\nfirst: %s\nlater: %s",
				i, first, got)
		}
	}

	var named []string
	for _, part := range strings.Split(strings.TrimPrefix(first, "ANOMALIES: "), "; ") {
		named = append(named, strings.Fields(part)[0])
	}
	const want = "dep-audit,doc-review,style-review"
	if got := strings.Join(named, ","); got != want {
		t.Fatalf("anomalies order:\n got: %s\nwant: %s\nline: %s", got, want, first)
	}
}
