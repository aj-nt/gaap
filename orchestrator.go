package gaap

import (
	"context"
	"fmt"
	"log/slog"
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
	cfg        *Config
	ctx        context.Context
	daemon     MnemoClient
	decomposer *Decomposer
	dag        *DAG
	state      OrchestratorState
	goal       string
}

// NewOrchestrator creates an orchestrator with the given config and daemon connection.
func NewOrchestrator(ctx context.Context, cfg *Config, daemon MnemoClient, decomposer *Decomposer) *Orchestrator {
	if cfg.PollIntervalSec <= 0 {
		cfg.PollIntervalSec = 5
	}
	if cfg.MaxWaitSec <= 0 {
		cfg.MaxWaitSec = 300
	}
	return &Orchestrator{
		cfg:        cfg,
		ctx:        ctx,
		daemon:     daemon,
		decomposer: decomposer,
		dag:        NewDAG(),
		state:      &IdleState{},
	}
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

	// Polling loop (simplified: single iteration for tests; real loop in CLI)
	if o.state.Name() == "waiting" {
		// Mark all leaf tasks as completed (simulation for testing)
		o.simulateCompletions()
	}

	return nil
}

// simulateCompletions marks all ready/leaf tasks as done and advances the DAG.
// This is a testing aid; the real orchestrator polls via pub/sub or daemon queries.
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
				o.Transition(next)
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
				o.Transition(next)
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
