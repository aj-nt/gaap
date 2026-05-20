package worker

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	vclient "github.com/aj-nt/vassago-sdk/client"
	pb "github.com/aj-nt/vassago-sdk/proto"

	"github.com/aj-nt/gaap/internal/ollama"
)

// mockMnemo embeds NullMnemo and overrides the task-management methods
// that pool.go calls: RegisterAgent, FindReadyTasks, ClaimTask, CompleteTask,
// FailTask, AddMemory.
type mockMnemo struct {
	vclient.NullMnemo

	tasks       []*vclient.TaskEntry // pre-configured task queue
	mu          sync.Mutex
	completed   []string // task IDs completed
	failed      []string // task IDs failed
	claimed     map[string]bool
	registerErr error
	claimErr    error
	findErr     error
	addMemErr   error

	// call tracking
	registerCalls int
	findCalls     int
	claimCalls    int
}

func (m *mockMnemo) RegisterAgent(ctx context.Context, agentID, name, role string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.registerCalls++
	if m.registerErr != nil {
		return "", false, m.registerErr
	}
	return "test-agent-id", true, nil
}

func (m *mockMnemo) FindReadyTasks(ctx context.Context, agentType string, limit int32) ([]*vclient.TaskEntry, error) {
	m.mu.Lock()
	m.findCalls++
	err := m.findErr
	m.mu.Unlock()

	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.tasks) == 0 {
		return nil, nil
	}
	task := m.tasks[0]
	m.tasks = m.tasks[1:]
	return []*vclient.TaskEntry{task}, nil
}

func (m *mockMnemo) ClaimTask(ctx context.Context, taskID, agentID string) (*vclient.TaskEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.claimCalls++
	if m.claimErr != nil {
		return nil, m.claimErr
	}
	if m.claimed == nil {
		m.claimed = make(map[string]bool)
	}
	if m.claimed[taskID] {
		return &vclient.TaskEntry{Id: taskID, Status: "claimed"}, nil
	}
	m.claimed[taskID] = true
	return &vclient.TaskEntry{Id: taskID, Status: "claimed", Goal: "test goal", AgentType: "test"}, nil
}

func (m *mockMnemo) CompleteTask(ctx context.Context, taskID, resultKey string) (*vclient.TaskEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.completed = append(m.completed, taskID)
	return &vclient.TaskEntry{Id: taskID, Status: "completed"}, nil
}

func (m *mockMnemo) FailTask(ctx context.Context, taskID string) (*vclient.TaskEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failed = append(m.failed, taskID)
	return &vclient.TaskEntry{Id: taskID, Status: "failed"}, nil
}

func (m *mockMnemo) AddMemory(ctx context.Context, target, category, key, content string, priority int32, sourceAgent string) (*pb.MemoryEntry, error) {
	if m.addMemErr != nil {
		return nil, m.addMemErr
	}
	// Return a pointer whose Id is non-empty so workerLoop prefers it over resultKey.
	return &pb.MemoryEntry{Id: "mem-" + key}, nil
}

func (m *mockMnemo) completedIDs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.completed))
	copy(out, m.completed)
	return out
}

func (m *mockMnemo) failedIDs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.failed))
	copy(out, m.failed)
	return out
}

// TestPoolWorkerLoopSuccess verifies the full lifecycle: find ready, claim,
// execute (DONE), CompleteTask, stats increment.
func TestPoolWorkerLoopSuccess(t *testing.T) {
	mnemo := &mockMnemo{
		tasks: []*vclient.TaskEntry{
			{Id: "task-1", Goal: "scan for bugs", AgentType: "static_analysis"},
		},
	}

	mockLLM := &mockChatClient{
		responses: []string{"DONE: scan complete, found 3 issues"},
	}
	exec := NewExecutor(mockLLM, 5)

	p := &Pool{
		cfg: PoolConfig{
			AgentTypes:  []string{"static_analysis"},
			WorkerCount: 1,
			PollSec:     1,
			RepoPath:    "/tmp",
		},
		mnemo:      mnemo,
		exec:       exec,
		agentID:    "test-agent",
		activeJobs: make(map[string]struct{}),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopCh := make(chan struct{})

	go func() {
		time.Sleep(2 * time.Second)
		close(stopCh)
	}()

	p.Run(ctx, stopCh)

	completed, failed := p.Stats()
	if completed != 1 {
		t.Errorf("expected 1 completed, got %d", completed)
	}
	if failed != 0 {
		t.Errorf("expected 0 failed, got %d", failed)
	}
	if ids := mnemo.completedIDs(); len(ids) != 1 || ids[0] != "task-1" {
		t.Errorf("expected completed [task-1], got %v", ids)
	}
}

// TestPoolWorkerLoopFail verifies FAIL -> FailTask -> failed counter.
func TestPoolWorkerLoopFail(t *testing.T) {
	mnemo := &mockMnemo{
		tasks: []*vclient.TaskEntry{
			{Id: "task-2", Goal: "deep scan", AgentType: "quality_scan"},
		},
	}

	mockLLM := &mockChatClient{
		responses: []string{"FAIL: repo not found"},
	}
	exec := NewExecutor(mockLLM, 5)

	p := &Pool{
		cfg: PoolConfig{
			AgentTypes:  []string{"quality_scan"},
			WorkerCount: 1,
			PollSec:     1,
			RepoPath:    "/tmp",
		},
		mnemo:      mnemo,
		exec:       exec,
		agentID:    "test-agent",
		activeJobs: make(map[string]struct{}),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopCh := make(chan struct{})

	go func() {
		time.Sleep(2 * time.Second)
		close(stopCh)
	}()

	p.Run(ctx, stopCh)

	completed, failed := p.Stats()
	if completed != 0 {
		t.Errorf("expected 0 completed, got %d", completed)
	}
	if failed != 1 {
		t.Errorf("expected 1 failed, got %d", failed)
	}
	if ids := mnemo.failedIDs(); len(ids) != 1 || ids[0] != "task-2" {
		t.Errorf("expected failed [task-2], got %v", ids)
	}
}

// TestPoolWorkerLoopNoTasks verifies the pool runs without error when no
// tasks are available (idle polling loop).
func TestPoolWorkerLoopNoTasks(t *testing.T) {
	mnemo := &mockMnemo{
		tasks: nil,
	}

	mockLLM := &mockChatClient{}
	exec := NewExecutor(mockLLM, 5)

	p := &Pool{
		cfg: PoolConfig{
			AgentTypes:  []string{"static_analysis"},
			WorkerCount: 1,
			PollSec:     1,
			RepoPath:    "/tmp",
		},
		mnemo:      mnemo,
		exec:       exec,
		agentID:    "test-agent",
		activeJobs: make(map[string]struct{}),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopCh := make(chan struct{})

	go func() {
		time.Sleep(1500 * time.Millisecond)
		close(stopCh)
	}()

	p.Run(ctx, stopCh)

	completed, failed := p.Stats()
	if completed != 0 || failed != 0 {
		t.Errorf("expected 0/0, got %d/%d", completed, failed)
	}
}

// TestPoolContextCancellation verifies the pool exits when the context is
// cancelled (without stopCh being closed).
func TestPoolContextCancellation(t *testing.T) {
	mnemo := &mockMnemo{
		tasks: []*vclient.TaskEntry{
			{Id: "task-3", Goal: "slow scan", AgentType: "static_analysis"},
		},
	}

	mockLLM := &mockChatClient{}
	exec := NewExecutor(mockLLM, 5)

	p := &Pool{
		cfg: PoolConfig{
			AgentTypes:  []string{"static_analysis"},
			WorkerCount: 1,
			PollSec:     1,
			RepoPath:    "/tmp",
		},
		mnemo:      mnemo,
		exec:       exec,
		agentID:    "test-agent",
		activeJobs: make(map[string]struct{}),
	}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		p.Run(ctx, nil)
		close(done)
	}()

	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// pool exited cleanly
	case <-time.After(5 * time.Second):
		t.Fatal("pool did not exit after context cancellation")
	}
}

// TestPoolStats verifies Stats() returns correct counters under concurrent access.
func TestPoolStats(t *testing.T) {
	t.Parallel()

	p := &Pool{
		mu:         sync.Mutex{},
		completed:  3,
		failed:     1,
		activeJobs: make(map[string]struct{}),
	}
	p.activeJobs["job-1"] = struct{}{}

	completed, failed := p.Stats()
	if completed != 3 {
		t.Errorf("expected 3 completed, got %d", completed)
	}
	if failed != 1 {
		t.Errorf("expected 1 failed, got %d", failed)
	}
}

// TestPoolIsIdle verifies IsIdle returns true only when no jobs are active.
func TestPoolIsIdle(t *testing.T) {
	t.Parallel()

	p := &Pool{
		mu:         sync.Mutex{},
		activeJobs: make(map[string]struct{}),
	}
	if !p.IsIdle() {
		t.Error("expected idle with no active jobs")
	}

	p.activeJobs["job-1"] = struct{}{}
	if p.IsIdle() {
		t.Error("expected NOT idle with active job")
	}
}

// TestNewPoolDefaults verifies default values when config fields are zero/empty.
func TestNewPoolDefaults(t *testing.T) {
	mnemo := &mockMnemo{}

	cfg := PoolConfig{
		Ollama: ollama.Config{Model: "test-model", BaseURL: "http://localhost:11434/v1"},
	}
	p, err := NewPool(context.Background(), cfg, mnemo)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	if len(p.cfg.AgentTypes) != 2 {
		t.Errorf("expected 2 default agent types, got %d", len(p.cfg.AgentTypes))
	}
	if p.cfg.WorkerCount != 2 {
		t.Errorf("expected 2 default workers, got %d", p.cfg.WorkerCount)
	}
	if p.cfg.PollSec != 2 {
		t.Errorf("expected 2s default poll, got %d", p.cfg.PollSec)
	}
	if p.exec.maxTurns != 20 {
		t.Errorf("expected 20 default max turns, got %d", p.exec.maxTurns)
	}
	if p.agentID != "test-agent-id" {
		t.Errorf("expected agentID 'test-agent-id', got %q", p.agentID)
	}
	if mnemo.registerCalls != 1 {
		t.Errorf("expected 1 RegisterAgent call, got %d", mnemo.registerCalls)
	}
}

// TestNewPoolRegisterAgentError verifies NewPool surfaces registration errors.
func TestNewPoolRegisterAgentError(t *testing.T) {
	mnemo := &mockMnemo{
		registerErr: fmt.Errorf("daemon unreachable"),
	}

	cfg := PoolConfig{
		Ollama: ollama.Config{Model: "test-model", BaseURL: "http://localhost:11434/v1"},
	}
	_, err := NewPool(context.Background(), cfg, mnemo)
	if err == nil {
		t.Fatal("expected error from NewPool when registration fails")
	}
	if err.Error() != "register agent: daemon unreachable" {
		t.Errorf("unexpected error: %v", err)
	}
}
