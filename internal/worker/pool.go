package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/aj-nt/gaap/internal/ollama"
	"github.com/aj-nt/vassago-sdk/client"
	"github.com/google/uuid"
)

// Pool runs a set of worker goroutines that poll the daemon for ready tasks,
// claim them atomically, execute via the Executor, and post results back.
type Pool struct {
	cfg        PoolConfig
	mnemo      client.MnemoClient
	exec       *Executor
	agentID    string
	mu         sync.Mutex
	completed  int
	failed     int
	activeJobs map[string]struct{} // task IDs currently being executed
}

// PoolConfig configures a worker pool.
type PoolConfig struct {
	DaemonAddr  string
	AgentID     string
	AgentName   string
	AgentTypes  []string
	WorkerCount int
	PollSec     int
	MaxTurns    int
	RepoPath    string
	Ollama      ollama.Config
}

// NewPool creates a worker pool and connects to the daemon.
// workersPerType goroutines are spawned per agent type.
func NewPool(ctx context.Context, cfg PoolConfig, mnemo client.MnemoClient) (*Pool, error) {
	if len(cfg.AgentTypes) == 0 {
		cfg.AgentTypes = []string{"static_analysis", "quality_scan"}
	}
	if cfg.WorkerCount <= 0 {
		cfg.WorkerCount = 2
	}
	if cfg.PollSec <= 0 {
		cfg.PollSec = 2
	}

	ollamaClient := ollama.NewClient(cfg.Ollama)
	exec := NewExecutor(ollamaClient, cfg.MaxTurns)

	// Register agent
	agentID, _, err := mnemo.RegisterAgent(ctx, cfg.AgentID, cfg.AgentName, "worker")
	if err != nil {
		return nil, fmt.Errorf("register agent: %w", err)
	}

	return &Pool{
		cfg:        cfg,
		mnemo:      mnemo,
		exec:       exec,
		agentID:    agentID,
		activeJobs: make(map[string]struct{}),
	}, nil
}

// Stats returns the current completed and failed counts.
func (p *Pool) Stats() (completed, failed int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.completed, p.failed
}

// IsIdle returns true if no tasks are currently being executed.
func (p *Pool) IsIdle() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.activeJobs) == 0
}

// Run starts the worker pool and blocks until ctx is cancelled,
// or stopCh is closed. Use stopCh to stop gracefully after DAG completion.
func (p *Pool) Run(ctx context.Context, stopCh <-chan struct{}) {
	var wg sync.WaitGroup

	// Spawn workers per agent type, round-robin across WorkerCount
	for i := 0; i < p.cfg.WorkerCount; i++ {
		at := p.cfg.AgentTypes[i%len(p.cfg.AgentTypes)]
		wg.Add(1)
		go func(agentType string) {
			defer wg.Done()
			p.workerLoop(ctx, stopCh, agentType)
		}(at)
	}

	slog.Info("worker pool started",
		"agent_id", p.agentID,
		"types", p.cfg.AgentTypes,
		"count", p.cfg.WorkerCount,
	)

	wg.Wait()
	slog.Info("worker pool stopped",
		"completed", p.completed,
		"failed", p.failed,
	)
}

// workerLoop polls for ready tasks of a specific agent type, claims, executes.
func (p *Pool) workerLoop(ctx context.Context, stopCh <-chan struct{}, agentType string) {
	pollInterval := time.Duration(p.cfg.PollSec) * time.Second
	// Short initial poll to pick up tasks immediately after dispatch
	initialPoll := 500 * time.Millisecond

	// First poll: quick wait then try
	select {
	case <-ctx.Done():
		return
	case <-stopCh:
		return
	case <-time.After(initialPoll):
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-stopCh:
			return
		default:
		}

		tasks, err := p.mnemo.FindReadyTasks(ctx, agentType, 1)
		if err != nil {
			slog.Warn("worker: FindReadyTasks failed", "agent_type", agentType, "error", err)
			select {
			case <-ctx.Done():
				return
			case <-stopCh:
				return
			case <-time.After(pollInterval):
			}
			continue
		}
		if len(tasks) == 0 {
			select {
			case <-ctx.Done():
				return
			case <-stopCh:
				return
			case <-time.After(pollInterval):
			}
			continue
		}

		task := tasks[0]
		tid := task.Id

		claimed, err := p.mnemo.ClaimTask(ctx, tid, p.agentID)
		if err != nil {
			slog.Warn("worker: ClaimTask failed", "id", tid, "error", err)
			continue
		}
		if claimed == nil || claimed.Status != "claimed" {
			slog.Info("worker: task already claimed", "id", tid)
			continue
		}

		p.mu.Lock()
		p.activeJobs[tid] = struct{}{}
		p.mu.Unlock()

		slog.Info("worker: executing task", "id", tid, "type", agentType, "goal", truncate(claimed.Goal, 100))

		result := p.exec.Execute(ctx, claimed, p.cfg.RepoPath)
		resultKey := fmt.Sprintf("result_%s_%s", tid, uuid.New().String()[:8])

		// Post result to memory blackboard
		resultJSON, _ := json.Marshal(result)
		if _, err := p.mnemo.AddMemory(ctx, "memory", "task_result", resultKey,
			string(resultJSON), 4, p.agentID); err != nil {
			slog.Warn("worker: failed to store result", "id", tid, "error", err)
		}

		if result.Status == "success" {
			if _, err := p.mnemo.CompleteTask(ctx, tid, resultKey); err != nil {
				slog.Warn("worker: failed to complete task", "id", tid, "error", err)
			}
			p.mu.Lock()
			p.completed++
			p.mu.Unlock()
		} else {
			if _, err := p.mnemo.FailTask(ctx, tid); err != nil {
				slog.Warn("worker: failed to mark task failed", "id", tid, "error", err)
			}
			p.mu.Lock()
			p.failed++
			p.mu.Unlock()
		}

		p.mu.Lock()
		delete(p.activeJobs, tid)
		p.mu.Unlock()

		slog.Info("worker: task done", "id", tid, "status", result.Status,
			"completed", p.completed, "failed", p.failed)
	}
}
