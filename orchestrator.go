package gaap

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/aj-nt/gaap/internal/worker"
)

// Config holds the orchestrator configuration.
type Config struct {
	DaemonAddr      string `yaml:"daemon_addr"`
	RepoPath        string `yaml:"repo_path"`
	MaxWaitSec      int    `yaml:"max_wait_sec"`
	PollIntervalSec int    `yaml:"poll_interval_sec"`
}

// RunState is a serializable snapshot of an orchestrator run that can be
// stored to and loaded from the daemon. It captures just enough to rebuild
// the DAG and resume from the Waiting state after an orchestrator crash.
type RunState struct {
	Goal     string     `json:"goal"`
	RepoPath string     `json:"repo_path"`
	Tasks    []TaskSpec `json:"tasks"`
}

// Orchestrator coordinates the full pipeline: decompose -> dispatch -> poll -> synthesize -> publish.
// It uses the State Pattern internally; phases are encapsulated as state objects.
// When a WorkerPool is configured, workers execute dispatched tasks concurrently — making
// the orchestrator self-contained rather than requiring external worker processes.
type Orchestrator struct {
	cfg             *Config
	ctx             context.Context
	daemon          MnemoClient
	decomposer      *Decomposer
	synthesis       *SynthesisEngine
	dag             *DAG
	state           OrchestratorState
	goal            string
	result          *SynthesisResult
	breakerRegistry map[string]*CircuitBreaker
	workerPool      *worker.Pool
	runKey          string // daemon key for persisted RunState
}

// NewOrchestrator creates an orchestrator with the given config and daemon connection.
func NewOrchestrator(ctx context.Context, cfg *Config, daemon MnemoClient, decomposer *Decomposer) *Orchestrator {
	if cfg.PollIntervalSec <= 0 {
		cfg.PollIntervalSec = 5
	}
	if cfg.MaxWaitSec <= 0 {
		cfg.MaxWaitSec = 300
	}
	o := &Orchestrator{
		cfg:        cfg,
		ctx:        ctx,
		daemon:     daemon,
		decomposer: decomposer,
		synthesis:  NewSynthesisEngine(nil), // schema-only by default; set later for LLM
		dag:        NewDAG(),
		state:      &IdleState{},
	}
	return o
}

// SetSynthesisChatFn configures the LLM-based synthesis strategy.
// When set, the synthesis engine will attempt LLM synthesis first,
// falling back to schema-based cross-reference on failure.
// Pass nil to use schema-only synthesis.
func (o *Orchestrator) SetSynthesisChatFn(chatFn func(ctx context.Context, prompt string) (string, error)) {
	o.synthesis = NewSynthesisEngine(chatFn)
}

// SetWorkerPool configures a worker pool that executes dispatched tasks in-process.
// When set, the orchestrator starts workers before the polling phase and stops them
// when all tasks complete. Nil disables auto-workers (requires external worker processes).
func (o *Orchestrator) SetWorkerPool(pool *worker.Pool) {
	o.workerPool = pool
}

// Run executes the orchestrator pipeline for a given goal.
func (o *Orchestrator) Run(goal string) error {
	if goal == "" {
		return fmt.Errorf("goal is required")
	}

	slog.Info("orchestrator starting",
		"goal", goal,
		"repo", o.cfg.RepoPath,
	)

	// Kick off: IdleState → PlanningState
	transition, err := o.state.HandleEvent(o.ctx, o, Event{
		Type:    EventGoalReceived,
		Payload: map[string]any{"goal": goal},
	})
	if err != nil {
		return fmt.Errorf("idle handler: %w", err)
	}
	if transition != nil {
		if err := o.Transition(transition); err != nil {
			return err
		}
	}

	// Planning completes synchronously and transitions to Waiting.
	if o.state.Name() == "planning" {
		transition, err := o.state.HandleEvent(o.ctx, o, Event{
			Type: EventTasksCreated,
		})
		if err != nil {
			return fmt.Errorf("planning handler: %w", err)
		}
		if transition != nil {
			if err := o.Transition(transition); err != nil {
				return err
			}
		}
	}

	return o.runWaitingAndBeyond()
}

// Resume rebuilds the orchestrator's DAG from a saved RunState, skips the plan
// phase, and resumes from the Waiting state. This implements crash recovery:
// the daemon still has all task state; we just need to reconnect the DAG and
// continue polling.
func (o *Orchestrator) Resume(rs *RunState) error {
	if rs == nil {
		return fmt.Errorf("run state is nil")
	}
	if rs.Goal == "" {
		return fmt.Errorf("run state has no goal")
	}

	o.goal = rs.Goal
	if rs.RepoPath != "" {
		o.cfg.RepoPath = rs.RepoPath
	}

	// Rebuild DAG from saved task specs.
	o.dag = NewDAG()
	for _, t := range rs.Tasks {
		err := o.dag.AddTask(&TaskNode{
			ID:        t.TaskID,
			ParentIDs: t.ParentIDs,
			Status:    t.Status,
			Goal:      t.Goal,
			AgentType: t.AgentType,
			Context:   t.Context,
		})
		if err != nil {
			return fmt.Errorf("resume: add task %s: %w", t.TaskID, err)
		}
	}

	slog.Info("orchestrator resuming from saved state",
		"goal", o.goal,
		"repo", o.cfg.RepoPath,
		"task_count", o.dag.TaskCount(),
	)

	// Jump directly to Waiting state — planning and dispatch are already done.
	o.state = &WaitingState{}

	return o.runWaitingAndBeyond()
}

// runWaitingAndBeyond runs the shared post-planning pipeline: start workers,
// poll daemon, synthesize, publish result.
func (o *Orchestrator) runWaitingAndBeyond() error {
	// Polling loop — query daemon for task completions, advance DAG.
	// Workers (if configured) execute tasks concurrently; pollOnce checks status.
	// Falls back to simulation when daemon is unavailable (NullMnemo).
	if o.state.Name() == "waiting" {
		// Start workers if configured
		var workerStopCh chan struct{}
		if o.workerPool != nil {
			workerStopCh = make(chan struct{})
			go o.workerPool.Run(o.ctx, workerStopCh)
			slog.Info("auto-workers started")
		}

		o.pollDaemon()

		// Stop workers
		if workerStopCh != nil {
			close(workerStopCh)
			slog.Info("auto-workers stopped")
		}
	}

	// Synthesizing completes synchronously and transitions to Done
	if o.state.Name() == "synthesizing" {
		transition, err := o.state.HandleEvent(o.ctx, o, Event{
			Type: EventTasksComplete,
		})
		if err != nil {
			return fmt.Errorf("synthesizing handler: %w", err)
		}
		if transition != nil {
			if err := o.Transition(transition); err != nil {
				return err
			}
		}
	}

	return nil
}

// SaveRunState persists the orchestrator's current DAG as a RunState to the
// daemon. This allows crash recovery via Resume(). The returned key is the
// daemon memory key that can be passed to gaap resume.
func (o *Orchestrator) SaveRunState(ctx context.Context, runKey string) error {
	rs := &RunState{
		Goal:     o.goal,
		RepoPath: o.cfg.RepoPath,
		Tasks:    make([]TaskSpec, 0, len(o.dag.nodes)),
	}
	for _, node := range o.dag.nodes {
		rs.Tasks = append(rs.Tasks, TaskSpec{
			TaskID:    node.ID,
			ParentIDs: node.ParentIDs,
			Status:    node.Status,
			Goal:      node.Goal,
			AgentType: node.AgentType,
			Context:   node.Context,
		})
	}

	payload, err := json.Marshal(rs)
	if err != nil {
		return fmt.Errorf("marshal run state: %w", err)
	}

	_, err = o.daemon.AddMemory(ctx, "memory", "orchestration_run", runKey,
		string(payload), 5, "gaap-orchestrator")
	if err != nil {
		return fmt.Errorf("save run state to daemon: %w", err)
	}

	o.runKey = runKey
	slog.Info("run state saved to daemon", "key", runKey, "task_count", len(rs.Tasks))
	return nil
}

// RunKey returns the saved daemon key for this run state.
func (o *Orchestrator) RunKey() string {
	return o.runKey
}

// LoadRunState fetches a RunState from the daemon by key.
func LoadRunState(ctx context.Context, daemon MnemoClient, runKey string) (*RunState, error) {
	entry, err := daemon.GetMemory(ctx, runKey)
	if err != nil {
		return nil, fmt.Errorf("get run state from daemon: %w", err)
	}
	if entry == nil {
		return nil, fmt.Errorf("run state not found: %s", runKey)
	}

	var rs RunState
	if err := json.Unmarshal([]byte(entry.Content), &rs); err != nil {
		return nil, fmt.Errorf("parse run state JSON: %w", err)
	}
	return &rs, nil
}

// pollDaemon queries the daemon for task completions, advancing the DAG when
// tasks transition to done. Falls back to simulation (mark-all-done) when the
// daemon is unavailable.
func (o *Orchestrator) pollDaemon() {
	// Detection: try one daemon query. If NullMnemo (returns "not connected"),
	// fall back to instant simulation (used by tests and dry runs).
	_, err := o.daemon.GetTask(o.ctx, "probe-detection")
	if err != nil && err.Error() == "vassago not connected" {
		slog.Info("polling: daemon unavailable, using simulation fallback")
		o.simulateCompletions()
		return
	}

	start := time.Now()
	pollInterval := time.Duration(o.cfg.PollIntervalSec) * time.Second
	maxWait := time.Duration(o.cfg.MaxWaitSec) * time.Second

	slog.Info("polling: querying daemon for task completions",
		"task_count", o.dag.TaskCount(),
		"poll_interval_sec", o.cfg.PollIntervalSec,
		"max_wait_sec", o.cfg.MaxWaitSec,
	)

	for {
		// Query all dispatched leaf tasks
		o.pollOnce()

		// Check if all done
		if o.dag.AllTasksComplete() {
			return
		}

		if time.Since(start) > maxWait {
			slog.Warn("polling: timeout waiting for workers",
				"elapsed_sec", int(time.Since(start).Seconds()))
			return
		}

		select {
		case <-o.ctx.Done():
			slog.Info("polling: context cancelled")
			return
		case <-time.After(pollInterval):
		}
	}
}

// pollOnce queries the daemon for each dispatched task, fires completion
// events, and advances the DAG. It also auto-completes synthesis tasks that
// were promoted to ready (synthesis runs in-process, not on workers).
func (o *Orchestrator) pollOnce() {
	for _, node := range o.dag.nodes {
		// Only query tasks that were dispatched to the daemon.
		if node.Status == "ready" || node.Status == "claimed" {
			entry, err := o.daemon.GetTask(o.ctx, node.ID)
			if err != nil {
				slog.Warn("poll: failed to get task", "id", node.ID, "error", err)
				continue
			}
			if entry == nil {
				continue
			}
			if entry.Status == "done" || entry.Status == "dead_letter" {
				node.Status = "done"

				// Read the worker's result from the daemon.
				// Workers store ExecuteResult JSON and pass the memory UUID
				// via CompleteTask; ResultKey now holds that UUID.
				if entry.ResultKey != "" {
					if mem, err := o.daemon.GetMemory(o.ctx, entry.ResultKey); err == nil && mem != nil {
						var wr worker.ExecuteResult
						if json.Unmarshal([]byte(mem.Content), &wr) == nil {
							node.Findings = wr.Findings
							node.Summary = wr.Summary
						}
					}
				}

				evt := Event{
					Type:    EventTaskCompleted,
					Payload: map[string]any{"task_id": node.ID},
				}
				slog.Info("poll: task completed", "id", node.ID)
				next, err := o.state.HandleEvent(o.ctx, o, evt)
				if err != nil {
					slog.Warn("completion handler error", "task_id", node.ID, "error", err)
				}
				if next != nil {
					if err := o.Transition(next); err != nil {
						slog.Warn("state transition failed", "error", err)
					}
				}
			}
		}
	}

	// Auto-complete synthesis tasks that were promoted to ready.
	// Synthesis runs in-process after all leaves complete, so there's no
	// daemon task to poll -- we mark them done and fire completion events
	// so the state machine can advance.
	for _, node := range o.dag.nodes {
		if node.AgentType == "synthesis" && node.Status == "ready" {
			node.Status = "done"
			slog.Info("poll: synthesis task auto-completed (in-process)", "id", node.ID)
			evt := Event{
				Type:    EventTaskCompleted,
				Payload: map[string]any{"task_id": node.ID},
			}
			next, err := o.state.HandleEvent(o.ctx, o, evt)
			if err != nil {
				slog.Warn("synthesis completion handler error", "task_id", node.ID, "error", err)
			}
			if next != nil {
				if err := o.Transition(next); err != nil {
					slog.Warn("state transition failed", "error", err)
				}
			}
		}
	}
}

// simulateCompletions marks all ready/leaf tasks as done and advances the DAG.
// Used by tests and when the daemon is unavailable (NullMnemo).
func (o *Orchestrator) simulateCompletions() {
	// First pass: mark all ready tasks as done.
	for id, node := range o.dag.nodes {
		if node.Status == "ready" && len(node.ParentIDs) == 0 {
			node.Status = "done"
			evt := Event{
				Type:    EventTaskCompleted,
				Payload: map[string]any{"task_id": id},
			}
			next, err := o.state.HandleEvent(o.ctx, o, evt)
			if err != nil {
				slog.Warn("completion handler error", "task_id", id, "error", err)
			}
			if next != nil {
				if err := o.Transition(next); err != nil {
					slog.Warn("state transition failed", "error", err)
				}
			}
		}
	}

	// Second pass: now promoted tasks may be ready (was blocked, now all parents done).
	// Mark those as done too.
	for id, node := range o.dag.nodes {
		if node.Status == "ready" {
			node.Status = "done"
			evt := Event{
				Type:    EventTaskCompleted,
				Payload: map[string]any{"task_id": id},
			}
			next, err := o.state.HandleEvent(o.ctx, o, evt)
			if err != nil {
				slog.Warn("completion handler error", "task_id", id, "error", err)
			}
			if next != nil {
				if err := o.Transition(next); err != nil {
					slog.Warn("state transition failed", "error", err)
				}
			}
		}
	}
}

// Transition orchestrator to the next state.
func (o *Orchestrator) Transition(next OrchestratorState) error {
	slog.Info("orchestrator state transition",
		"from", o.state.Name(),
		"to", next.Name())
	o.state = next
	return next.Enter(o.ctx, o)
}
