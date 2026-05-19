package gaap

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// Config holds the orchestrator configuration.
type Config struct {
	DaemonAddr      string `yaml:"daemon_addr"`
	RepoPath        string `yaml:"repo_path"`
	MaxWaitSec      int    `yaml:"max_wait_sec"`
	PollIntervalSec int    `yaml:"poll_interval_sec"`
}

// Orchestrator coordinates the full pipeline: decompose -> dispatch -> poll -> synthesize -> publish.
// It uses the State Pattern internally; phases are encapsulated as state objects.
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

	// Polling loop — query daemon for task completions, advance DAG.
	// Falls back to simulation when daemon is unavailable (NullMnemo).
	if o.state.Name() == "waiting" {
		o.pollDaemon()
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
					_ = o.Transition(next)
				}
			}
		}
	}

	// Auto-complete synthesis tasks that were promoted to ready.
	// Synthesis runs in-process after all leaves complete, so there's no
	// daemon task to poll — we mark them done here to unblock the DAG.
	for _, node := range o.dag.nodes {
		if node.AgentType == "synthesis" && node.Status == "ready" {
			node.Status = "done"
			slog.Info("poll: synthesis task auto-completed (in-process)", "id", node.ID)
		}
	}
}

// simulateCompletions marks all ready/leaf tasks as done and advances the DAG.
// Used by tests and when the daemon is unavailable (NullMnemo).
func (o *Orchestrator) simulateCompletions() {
	// First pass: mark all ready tasks as done.
	// We walk the DAG's nodes map directly.
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
				_ = o.Transition(next)
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
				_ = o.Transition(next)
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
