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

	// script, when set, is what Subscribe streams instead of the task store.
	// Tests that build their DAG by hand have nothing in the store to replay.
	script []*pb.TaskEntry

	// memories is the blackboard: AddMemory stores under a deterministic ID so a
	// test can hand that ID to CompleteTask as a result key, exactly as a worker
	// does with the UUID the real daemon assigns.
	memories map[string]*pb.MemoryEntry
}

func (td *testDaemon) AddTask(_ context.Context, req *pb.AddTaskRequest) (*pb.TaskEntry, error) {
	td.mu.Lock()
	defer td.mu.Unlock()
	e := &pb.TaskEntry{
		Id:         req.Id,
		Goal:       req.Goal,
		AgentType:  req.AgentType,
		Context:    req.Context,
		Status:     "ready",
		Priority:   req.Priority,
		MaxRetries: req.MaxRetries,
		CreatedAt:  time.Now().Unix(),
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
	// The dead-letter list wins over autoComplete: a test that asks for a
	// failure wants the failure, not a simulated success that arrives first.
	if e.Status == "ready" {
		for name := range td.deadLetter {
			if strings.HasSuffix(e.Id, name) {
				e.Status = "dead_letter"
			}
		}
	}
	if td.autoComplete && e.Status == "ready" {
		e.Status = "done"
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

// tasksOfType returns the tasks currently in the store with the given agent
// type. Locked: a test polls this while the run is live.
func (td *testDaemon) tasksOfType(agentType string) []*pb.TaskEntry {
	td.mu.Lock()
	defer td.mu.Unlock()
	var out []*pb.TaskEntry
	for _, e := range td.tasks {
		if e.AgentType == agentType {
			out = append(out, &pb.TaskEntry{
				Id:         e.Id,
				AgentType:  e.AgentType,
				Goal:       e.Goal,
				Context:    e.Context,
				Status:     e.Status,
				MaxRetries: e.MaxRetries,
			})
		}
	}
	return out
}

// Subscribe emits the tasks currently in the store as task events, marking the
// ones in deadLetter as given up on, then closes the stream. Tasks are replayed
// repeatedly so the orchestrator's reader has real work to do while its loop
// inspects the DAG — the concurrency the single-writer rule has to survive.
func (td *testDaemon) Subscribe(_ *pb.SubscribeRequest, stream pb.Vassago_SubscribeServer) error {
	td.mu.Lock()
	if len(td.script) > 0 {
		entries := make([]*pb.TaskEntry, 0, len(td.script))
		for _, e := range td.script {
			entries = append(entries, &pb.TaskEntry{Id: e.Id, Status: e.Status, AgentType: e.AgentType, Goal: e.Goal})
		}
		td.mu.Unlock()
		return streamScript(stream, entries)
	}
	entries := make([]*pb.TaskEntry, 0, len(td.tasks))
	for _, e := range td.tasks {
		// proto messages embed a mutex, so build the event entry rather than
		// copying one.
		ev := &pb.TaskEntry{Id: e.Id, AgentType: e.AgentType, Goal: e.Goal, ResultKey: e.ResultKey, Status: "done"}
		for name := range td.deadLetter {
			if name != "" && strings.HasSuffix(ev.Id, name) {
				ev.Status = "dead_letter"
			}
		}
		entries = append(entries, ev)
	}
	td.mu.Unlock()

	return streamScript(stream, entries)
}

// streamScript replays entries repeatedly, so a reader has real work to do
// while the orchestrator's loop inspects the DAG.
func streamScript(stream pb.Vassago_SubscribeServer, entries []*pb.TaskEntry) error {
	for i := 0; i < 10; i++ {
		for _, e := range entries {
			if err := stream.Send(&pb.UpdateEvent{Task: e}); err != nil {
				return err
			}
		}
	}
	return nil
}

// setupDaemonScripted starts a daemon whose subscription replays script.
func setupDaemonScripted(t *testing.T, script ...*pb.TaskEntry) (client.MnemoClient, func()) {
	t.Helper()
	return serveTestDaemon(t, &testDaemon{tasks: map[string]*pb.TaskEntry{}, script: script})
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
	entry := &pb.MemoryEntry{
		Id:       "mem-" + req.Key,
		Category: req.Category,
		Key:      req.Key,
		Content:  req.Content,
		Priority: req.Priority,
	}
	if td.memories == nil {
		td.memories = map[string]*pb.MemoryEntry{}
	}
	td.memories[entry.Id] = entry
	return &pb.AddMemoryResponse{Entry: entry}, nil
}

func (td *testDaemon) DeleteTask(_ context.Context, req *pb.DeleteTaskRequest) (*pb.DeleteTaskResponse, error) {
	td.mu.Lock()
	defer td.mu.Unlock()
	if _, ok := td.tasks[req.Id]; !ok {
		return &pb.DeleteTaskResponse{Deleted: false}, nil
	}
	delete(td.tasks, req.Id)
	return &pb.DeleteTaskResponse{Deleted: true}, nil
}

func (td *testDaemon) GetMemory(_ context.Context, req *pb.GetMemoryRequest) (*pb.MemoryEntry, error) {
	td.mu.Lock()
	defer td.mu.Unlock()
	if e, ok := td.memories[req.Id]; ok {
		return e, nil
	}
	return nil, status.Error(codes.NotFound, "memory not found: "+req.Id)
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

	if orch.state == nil || orch.state.Phase() != PhaseDone {
		t.Errorf("expected state done, got %q", orch.state.Phase())
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
	if got := orch.state.Phase(); got != "failed" {
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
