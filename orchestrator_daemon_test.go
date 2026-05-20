package gaap

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

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
	mu          sync.Mutex
	tasks       map[string]*pb.TaskEntry
	autoComplete bool // if true, tasks become "done" on AddTask/ClaimTask
}

func newTestDaemon() *testDaemon {
	return &testDaemon{tasks: make(map[string]*pb.TaskEntry), autoComplete: true}
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

func setupDaemonWithAutoComplete(t *testing.T, auto bool) (client.MnemoClient, func()) {
	t.Helper()

	td := &testDaemon{tasks: make(map[string]*pb.TaskEntry), autoComplete: auto}
	lis := bufconn.Listen(256 * 1024)
	s := grpc.NewServer()
	pb.RegisterVassagoServer(s, td)
	go s.Serve(lis)
	t.Cleanup(s.Stop)

	conn, err := grpc.Dial("bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial bufconn: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

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

// TestOrchestrator_DeadLetter_TriggersFailedState verifies that a
// task that never completes triggers the FailedState after timeout.
func TestOrchestrator_DeadLetter_TriggersFailedState(t *testing.T) {
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
	_ = orch.Run("dead letter test")
	// Run returns nil even on failure — FailedState is graceful termination.
	if orch.state == nil || orch.state.Name() != "failed" {
		t.Errorf("expected state failed, got %q", orch.state.Name())
	}
}
