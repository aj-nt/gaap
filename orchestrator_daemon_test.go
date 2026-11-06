package gaap

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	gstatus "github.com/aj-nt/gaap/internal/status"

	"github.com/aj-nt/vassago-sdk/client"
	pb "github.com/aj-nt/vassago-sdk/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// testDaemon is a minimal in-memory Vassago gRPC server that overrides
// only the task-management methods the orchestrator calls.
type testDaemon struct {
	pb.UnimplementedVassagoServer
	mu           sync.Mutex
	tasks        map[string]*pb.TaskEntry
	autoComplete bool // if true, tasks become "done" on AddTask/ClaimTask

	// deadLetter lists task IDs the daemon gives up on: the first poll that
	// finds one ready returns it as dead_letter, exactly as FailTask would
	// after the retries ran out.
	deadLetter map[string]bool
}

func (td *testDaemon) AddTask(_ context.Context, req *pb.AddTaskRequest) (*pb.TaskEntry, error) {
	td.mu.Lock()
	defer td.mu.Unlock()
	e := &pb.TaskEntry{
		Id:        req.Id,
		Goal:      req.Goal,
		AgentType: req.AgentType,
		Status:    "ready",
		Priority:  req.Priority,
		CreatedAt: time.Now().Unix(),
	}
	td.tasks[req.Id] = e
	return e, nil
}

func (td *testDaemon) GetTask(_ context.Context, req *pb.GetTaskRequest) (*pb.TaskEntry, error) {
	td.mu.Lock()
	defer td.mu.Unlock()
	e, ok := td.tasks[req.Id]
	if !ok {
		return nil, status.Error(codes.NotFound, "task not found")
	}
	if td.autoComplete && e.Status == "ready" {
		e.Status = "done"
	}
	if e.Status == "ready" {
		for name := range td.deadLetter {
			if strings.HasSuffix(e.Id, name) {
				e.Status = "dead_letter"
			}
		}
	}
	return e, nil
}

func (td *testDaemon) ClaimTask(_ context.Context, req *pb.ClaimTaskRequest) (*pb.TaskEntry, error) {
	td.mu.Lock()
	defer td.mu.Unlock()
	e, ok := td.tasks[req.TaskId]
	if !ok {
		return nil, status.Error(codes.NotFound, "task not found")
	}
	if e.Status != "ready" {
		return nil, status.Error(codes.FailedPrecondition, "task not ready")
	}
	e.Status = "claimed"
	e.AssignedAgent = req.AgentId
	return e, nil
}

func (td *testDaemon) CompleteTask(_ context.Context, req *pb.CompleteTaskRequest) (*pb.TaskEntry, error) {
	td.mu.Lock()
	defer td.mu.Unlock()
	e, ok := td.tasks[req.TaskId]
	if !ok {
		return nil, status.Error(codes.NotFound, "task not found")
	}
	e.Status = "done"
	e.ResultKey = req.ResultKey
	return e, nil
}

func (td *testDaemon) FailTask(_ context.Context, req *pb.FailTaskRequest) (*pb.TaskEntry, error) {
	td.mu.Lock()
	defer td.mu.Unlock()
	e, ok := td.tasks[req.TaskId]
	if !ok {
		return nil, status.Error(codes.NotFound, "task not found")
	}
	e.Status = "dead_letter"
	return e, nil
}

func (td *testDaemon) FindReadyTasks(_ context.Context, req *pb.FindReadyTasksRequest) (*pb.TaskList, error) {
	td.mu.Lock()
	defer td.mu.Unlock()
	var ready []*pb.TaskEntry
	for _, e := range td.tasks {
		if e.AgentType == req.AgentType && e.Status == "ready" {
			ready = append(ready, e)
		}
	}
	if ready == nil {
		ready = []*pb.TaskEntry{}
	}
	return &pb.TaskList{Tasks: ready}, nil
}

func (td *testDaemon) AddMemory(_ context.Context, req *pb.AddMemoryRequest) (*pb.AddMemoryResponse, error) {
	td.mu.Lock()
	defer td.mu.Unlock()
	id := "mem-" + time.Now().Format("150405")
	return &pb.AddMemoryResponse{
		Entry: &pb.MemoryEntry{
			Id:       id,
			Category: req.Category,
			Key:      req.Key,
			Content:  req.Content,
			Priority: req.Priority,
		},
	}, nil
}

func (td *testDaemon) Ping(_ context.Context, _ *pb.HealthCheckRequest) (*pb.HealthCheckResponse, error) {
	return &pb.HealthCheckResponse{Status: "ok"}, nil
}

// setupTestDaemon starts a bufconn gRPC server and returns a MnemoClient.
// Tasks auto-complete on GetTask (useful for happy-path tests).
func setupTestDaemon(t *testing.T) (client.MnemoClient, func()) {
	t.Helper()
	return setupDaemonWithAutoComplete(t, true)
}

// setupDaemonNoAutoComplete starts a test daemon that does NOT auto-complete
// tasks — they stay "ready" until explicitly completed. Useful for timeout/
// dead-letter tests.
func setupDaemonNoAutoComplete(t *testing.T) (client.MnemoClient, func()) {
	t.Helper()
	return setupDaemonWithAutoComplete(t, false)
}

// setupDaemonDeadLettering starts a daemon that gives up on the named tasks.
func setupDaemonDeadLettering(t *testing.T, ids ...string) (client.MnemoClient, func()) {
	t.Helper()
	td := &testDaemon{tasks: make(map[string]*pb.TaskEntry), deadLetter: map[string]bool{}}
	for _, id := range ids {
		td.deadLetter[id] = true
	}
	return serveTestDaemon(t, td)
}

func setupDaemonWithAutoComplete(t *testing.T, auto bool) (client.MnemoClient, func()) {
	t.Helper()

	td := &testDaemon{tasks: make(map[string]*pb.TaskEntry), autoComplete: auto}
	return serveTestDaemon(t, td)
}

// serveTestDaemon runs td behind a bufconn listener and returns a client for it.
func serveTestDaemon(t *testing.T, td *testDaemon) (client.MnemoClient, func()) {
	t.Helper()

	lis := bufconn.Listen(256 * 1024)
	s := grpc.NewServer()
	pb.RegisterVassagoServer(s, td)
	go func() { _ = s.Serve(lis) }()
	t.Cleanup(s.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial bufconn: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	return client.NewClientFromConn(conn), func() { s.Stop() }
}

// TestOrchestratorAgainstRealDaemon validates the orchestrator against
// a live gRPC daemon over bufconn — real serialization, streaming, and
// state machine transitions.
func TestOrchestratorAgainstRealDaemon(t *testing.T) {
	daemon, cleanup := setupTestDaemon(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	cfg := &Config{
		MaxWaitSec:      10,
		PollIntervalSec: 1,
	}

	decomp := NewDecomposer(&StaticDecomposition{
		Tasks: []TaskSpec{
			{TaskID: "leaf-1", AgentType: "static_analysis", Status: "ready", Goal: "Run lint"},
			{TaskID: "leaf-2", AgentType: "quality_scan", Status: "ready", Goal: "Scan quality"},
			{TaskID: "synthesis", AgentType: "synthesis", Status: "blocked", Goal: "Synthesize", ParentIDs: []string{"leaf-1", "leaf-2"}},
		},
	})

	orch := NewOrchestrator(ctx, cfg, daemon, decomp)
	err := orch.Run("test goal")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if orch.state == nil || orch.state.Name() != "done" {
		t.Errorf("expected state done, got %q", orch.state.Name())
	}
	if !orch.dag.AllTasksComplete() {
		t.Error("not all tasks completed")
	}
	if orch.dag.TaskCount() != 3 {
		t.Errorf("expected 3 tasks, got %d", orch.dag.TaskCount())
	}
	if orch.result == nil {
		t.Error("synthesis result is nil")
	}
}

// TestOrchestrator_Timeout_TriggersFailedState covers the timeout path: a task
// that never completes (and is never dead-lettered either) ends the run in
// FailedState, and the run reports itself as failed.
func TestOrchestrator_Timeout_TriggersFailedState(t *testing.T) {
	daemon, cleanup := setupDaemonNoAutoComplete(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cfg := &Config{
		MaxWaitSec:      2, // Short — fails fast
		PollIntervalSec: 1,
	}

	decomp := NewDecomposer(&StaticDecomposition{
		Tasks: []TaskSpec{
			{TaskID: "will-fail", AgentType: "flaky_agent", Status: "ready", Goal: "Never completes"},
			{TaskID: "synthesis", AgentType: "synthesis", Status: "blocked", Goal: "Synthesize", ParentIDs: []string{"will-fail"}},
		},
	})

	orch := NewOrchestrator(ctx, cfg, daemon, decomp)
	err := orch.Run("timeout test")

	// The report is still produced, but the run must not claim success.
	if !errors.Is(err, ErrOrchestrationFailed) {
		t.Fatalf("Run err = %v, want ErrOrchestrationFailed", err)
	}
	if orch.state == nil {
		t.Fatal("state is nil")
	}
	if got := orch.state.Name(); got != "failed" {
		t.Errorf("expected state failed, got %q", got)
	}
}

// TestOrchestrator_DeadLetteredTaskFailsRun is the end-to-end version of the
// regression: over the real wire, the daemon gives up on a task, and the run
// must end failed with its dependent skipped — never done.
func TestOrchestrator_DeadLetteredTaskFailsRun(t *testing.T) {
	daemon, cleanup := setupDaemonDeadLettering(t, "will-fail")
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	cfg := &Config{MaxWaitSec: 10, PollIntervalSec: 1}

	decomp := NewDecomposer(&StaticDecomposition{
		Tasks: []TaskSpec{
			{TaskID: "will-fail", AgentType: "flaky_agent", Status: "ready", Goal: "Dies"},
			{TaskID: "dependant", AgentType: "static_analysis", Status: "blocked", Goal: "Never runs", ParentIDs: []string{"will-fail"}},
			{TaskID: "synthesis", AgentType: "synthesis", Status: "blocked", Goal: "Synthesize", ParentIDs: []string{"dependant"}},
		},
	})

	orch := NewOrchestrator(ctx, cfg, daemon, decomp)
	err := orch.Run("dead letter test")

	if !errors.Is(err, ErrOrchestrationFailed) {
		t.Fatalf("Run err = %v, want ErrOrchestrationFailed", err)
	}
	if got := nodeEndingIn(t, orch.dag, "will-fail").Status; got != gstatus.TaskFailed {
		t.Fatalf("will-fail = %s, want failed", got)
	}
	if got := nodeEndingIn(t, orch.dag, "dependant").Status; got != gstatus.TaskSkipped {
		t.Fatalf("dependant = %s, want skipped", got)
	}
	if got := nodeEndingIn(t, orch.dag, "synthesis").Status; got != gstatus.TaskSkipped {
		t.Fatalf("synthesis = %s, want skipped", got)
	}
	if orch.dag.AllTasksComplete() {
		t.Fatal("a dead-lettered task must never satisfy AllTasksComplete")
	}
}

// nodeEndingIn finds a DAG node by its logical name. Planning prefixes task IDs
// with a per-run token, so exact-ID lookups belong only in hand-built DAGs.
func nodeEndingIn(t *testing.T, d *DAG, suffix string) *TaskNode {
	t.Helper()
	for id, n := range d.nodes {
		if strings.HasSuffix(id, suffix) {
			return n
		}
	}
	t.Fatalf("no task ending in %q", suffix)
	return nil
}
