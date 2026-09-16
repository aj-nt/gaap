package gaap

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	vclient "github.com/aj-nt/vassago-sdk/client"
	pb "github.com/aj-nt/vassago-sdk/proto"
)

// ---- mock: records digests published via AddMemory ----

type recordingMnemo struct {
	vclient.NullMnemo

	mu       sync.Mutex
	calls    []memCall
	failNext bool
}

type memCall struct {
	Target      string
	Category    string
	Key         string
	Content     string
	Priority    int32
	SourceAgent string
}

func (m *recordingMnemo) AddMemory(ctx context.Context, target, category, key, content string, priority int32, sourceAgent string) (*pb.MemoryEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failNext {
		return nil, fmt.Errorf("daemon down")
	}
	m.calls = append(m.calls, memCall{target, category, key, content, priority, sourceAgent})
	return &pb.MemoryEntry{Id: key}, nil
}

func (m *recordingMnemo) callsInCategory(cat string) []memCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []memCall
	for _, c := range m.calls {
		if c.Category == cat {
			out = append(out, c)
		}
	}
	return out
}

// ---- fixtures ----

func digestTestOrchestrator(m MnemoClient) *Orchestrator {
	cfg := &Config{RepoPath: "/tmp/x"}
	o := NewOrchestrator(context.Background(), cfg, m, nil)
	o.goal = "audit package for quality"
	_ = o.dag.AddTask(&TaskNode{ID: "T1", Status: "ready", Goal: "run static analysis", AgentType: "security-scan"})
	_ = o.dag.AddTask(&TaskNode{ID: "T2", Status: "ready", Goal: "lint review", AgentType: "style-review"})
	_ = o.dag.AddTask(&TaskNode{ID: "T3", Status: "blocked", Goal: "cross review", AgentType: "doc-review", ParentIDs: []string{"T1"}})
	return o
}

func completeTask(o *Orchestrator, id, summary string) {
	node := o.dag.nodes[id]
	node.Status = "done"
	node.Summary = summary
}

// ---- digest core ----

func TestDigestBuild_ObserverAudienceContainsBlockedWithAttribution(t *testing.T) {
	t.Parallel()
	o := digestTestOrchestrator(&vclient.NullMnemo{})
	completeTask(o, "T1", "3 findings: HIGH SQL injection in query builder")
	// T3 still waiting, parent T1 now done

	d := o.buildDigest("observer")
	if !strings.Contains(d, "BLOCKED") || !strings.Contains(d, "blocked by T1") {
		t.Fatalf("observer digest must attribute blockers\ngot: %s", d)
	}
}

func TestDigestBuild_WorkerAudienceContainsErrors(t *testing.T) {
	t.Parallel()
	o := digestTestOrchestrator(&vclient.NullMnemo{})
	node := o.dag.nodes["T1"]
	node.Status = "failed"
	node.Summary = ""
	o.dag.nodes["T1"] = node

	d := o.buildDigest("worker")
	if !strings.Contains(d, "FAIL") || !strings.Contains(d, "T1") {
		t.Fatalf("worker digest must surface failures\ngot: %s", d)
	}
}

func TestDigestBuild_BudgetDropsWholeLinesAndDeclares(t *testing.T) {
	t.Parallel()
	o := digestTestOrchestrator(&vclient.NullMnemo{})
	completeTask(o, "T1", strings.Repeat("x", 400)) // force overflow

	orig := digestMaxChars
	digestMaxChars = 200
	defer func() { digestMaxChars = orig }()

	d := o.buildDigest("observer")
	if len(d) > 200+80 { // tolerance for the drop notice line
		t.Fatalf("digest must respect budget, got %d chars", len(d))
	}
	if !strings.Contains(d, "dropped") {
		t.Fatalf("digest must declare dropped lines\ngot: %s", d)
	}
	// structural line (header) must survive
	if !strings.Contains(d, "audit package for quality") {
		t.Fatalf("structural header must never be dropped\ngot: %s", d)
	}
}

func TestDigestBuild_TransportNoiseExcluded(t *testing.T) {
	t.Parallel()
	o := digestTestOrchestrator(&vclient.NullMnemo{})
	d := o.buildDigest("worker")
	for _, noise := range []string{"heartbeat", "lease"} {
		if strings.Contains(strings.ToLower(d), noise) {
			t.Fatalf("digest must not contain transport noise %q\ngot: %s", noise, d)
		}
	}
}

// ---- emission seam ----

func TestWaitingState_CompletionEmitsDigests(t *testing.T) {
	t.Parallel()
	m := &recordingMnemo{}
	o := digestTestOrchestrator(m)
	completeTask(o, "T1", "3 findings: HIGH SQL injection")

	_ = o.dag.PromoteToReady("T3") // simulate DAG advance
	ws := &WaitingState{}
	_, _ = ws.HandleEvent(context.Background(), o, Event{Type: EventTaskCompleted, Payload: map[string]any{"task_id": "T1"}})

	pub := m.callsInCategory("pipeline_digest")
	if len(pub) == 0 {
		t.Fatalf("completion must emit pipeline_digest")
	}
	for _, c := range pub {
		if c.Target != "push" {
			t.Fatalf("digest must publish to push target, got %q", c.Target)
		}
		if c.SourceAgent != "gaap-orchestrator" {
			t.Fatalf("digest source must be gaap-orchestrator, got %q", c.SourceAgent)
		}
	}
}

func TestWaitingState_EmitFailureIsNonFatal(t *testing.T) {
	t.Parallel()
	m := &recordingMnemo{failNext: true}
	o := digestTestOrchestrator(m)
	completeTask(o, "T1", "findings")
	ws := &WaitingState{}

	_, err := ws.HandleEvent(context.Background(), o, Event{Type: EventTaskCompleted, Payload: map[string]any{"task_id": "T1"}})
	if err != nil {
		t.Fatalf("digest publish failure must not fail DAG advancement, got: %v", err)
	}
	if len(m.calls) != 0 {
		t.Fatalf("failed publish must not record calls, got %d", len(m.calls))
	}
}

func TestDigestBuild_IncludesAnomalies(t *testing.T) {
	t.Parallel()
	m := &recordingMnemo{}
	o := digestTestOrchestrator(m)
	cb := NewCircuitBreaker("dep-audit", 3, 30*time.Second)
	for i := 0; i < 3; i++ {
		cb.RecordFailure()
	}
	o.breakerRegistry = map[string]*CircuitBreaker{"dep-audit": cb}

	d := o.buildDigest("observer")
	if !strings.Contains(d, "ANOMALIES") {
		t.Fatalf("observer digest must include anomalies section\ngot: %s", d)
	}
	if !strings.Contains(d, "dep-audit") || !strings.Contains(d, "open") {
		t.Fatalf("anomalies must name agent type and state\ngot: %s", d)
	}
	// healthy breakers are not anomalies — no section when all closed
	o2 := digestTestOrchestrator(m)
	o2.breakerRegistry = map[string]*CircuitBreaker{"ok-agent": NewCircuitBreaker("ok-agent", 3, time.Second)}
	d2 := o2.buildDigest("observer")
	if strings.Contains(d2, "ANOMALIES") {
		t.Fatalf("digest must omit anomalies section when all breakers closed\ngot: %s", d2)
	}
}
