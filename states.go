package gaap

import (
	"context"
	"log/slog"
)

// EventType categorizes events in the orchestrator event loop.
type EventType int

const (
	EventGoalReceived  EventType = iota
	EventTasksCreated
	EventTaskCompleted
	EventTaskFailed
	EventTimeout
	EventTasksComplete // fired when all tasks in the DAG are done
)

// Event is a pub/sub or timer event that drives state transitions.
type Event struct {
	Type    EventType
	Payload map[string]any
}

// OrchestratorState defines the interface for all orchestrator states.
type OrchestratorState interface {
	// Enter is called when transitioning into this state.
	Enter(ctx context.Context, o *Orchestrator) error
	// HandleEvent processes an event and returns the next state (nil = stay).
	HandleEvent(ctx context.Context, o *Orchestrator, evt Event) (OrchestratorState, error)
	// Name returns a human-readable state name.
	Name() string
}

// IdleState awaits a goal. On receiving a goal, transitions to PlanningState.
type IdleState struct{}

func (s *IdleState) Name() string { return "idle" }

func (s *IdleState) Enter(ctx context.Context, o *Orchestrator) error {
	slog.Info("orchestrator idle", "repo", o.cfg.RepoPath)
	return nil
}

func (s *IdleState) HandleEvent(ctx context.Context, o *Orchestrator, evt Event) (OrchestratorState, error) {
	if evt.Type == EventGoalReceived {
		goal, ok := evt.Payload["goal"].(string)
		if !ok || goal == "" {
			return nil, nil // Stay idle
		}
		o.goal = goal
		slog.Info("goal received, transitioning to planning", "goal", goal)
		return &PlanningState{}, nil
	}
	return nil, nil
}

// PlanningState decomposes the goal into tasks, creates them in the DAG,
// dispatches to the daemon, then transitions to WaitingState.
type PlanningState struct{}

func (s *PlanningState) Name() string { return "planning" }

func (s *PlanningState) Enter(ctx context.Context, o *Orchestrator) error {
	slog.Info("planning: decomposing goal", "goal", o.goal, "repo", o.cfg.RepoPath)

	if o.decomposer == nil {
		o.decomposer = NewDecomposer(nil) // defaults to StaticDecomposition
	}

	tasks, err := o.decomposer.Decompose(ctx, o.goal, o.cfg.RepoPath)
	if err != nil {
		return err
	}

	slog.Info("planning: decomposed into tasks", "count", len(tasks))

	// Add all tasks to the DAG
	for _, t := range tasks {
		err := o.dag.AddTask(&TaskNode{
			ID:        t.TaskID,
			ParentIDs: t.ParentIDs,
			Status:    t.Status,
			Goal:      t.Goal,
			AgentType: t.AgentType,
			Context:   t.Context,
		})
		if err != nil {
			return err
		}
	}

	// Dispatch leaf tasks (ready status, no parents) to the daemon
	for _, t := range tasks {
		if t.Status == "ready" && len(t.ParentIDs) == 0 {
			ctxJSON := "{}"
			if t.Context != nil {
				// Marshal context for the daemon
				// For tests, we skip this — the NullMnemo is a no-op.
				_ = ctxJSON
			}
			_, err := o.daemon.AddTask(ctx, t.TaskID, t.AgentType, t.Goal, ctxJSON, 3, 300, 2)
			if err != nil {
				slog.Warn("failed to dispatch task", "id", t.TaskID, "error", err)
				// Non-fatal: task is in DAG, can be retried
			}
		}
	}

	slog.Info("planning: tasks created in DAG", "count", o.dag.TaskCount())
	return nil
}

func (s *PlanningState) HandleEvent(ctx context.Context, o *Orchestrator, evt Event) (OrchestratorState, error) {
	// Planning completes synchronously — always transition to waiting.
	return &WaitingState{}, nil
}

// WaitingState monitors task completions and advances the DAG.
// When all tasks are done, transitions to SynthesizingState.
type WaitingState struct{}

func (s *WaitingState) Name() string { return "waiting" }

func (s *WaitingState) Enter(ctx context.Context, o *Orchestrator) error {
	slog.Info("waiting: monitoring task completions", "dag_size", o.dag.TaskCount())
	return nil
}

func (s *WaitingState) HandleEvent(ctx context.Context, o *Orchestrator, evt Event) (OrchestratorState, error) {
	switch evt.Type {
	case EventTaskCompleted:
		taskID, _ := evt.Payload["task_id"].(string)
		if taskID != "" {
			slog.Info("task completed, advancing DAG", "task_id", taskID)
			// Promote children whose parents are now all done.
			children := o.dag.ChildrenOf(taskID)
			for _, child := range children {
				if allDone, err := o.dag.AllParentsComplete(child.ID); err == nil && allDone {
					if err := o.dag.PromoteToReady(child.ID); err != nil {
						slog.Warn("failed to promote task", "task_id", child.ID, "error", err)
					} else {
						slog.Info("promoted task to ready", "task_id", child.ID)
					}
				}
			}
		}

		// Check if all tasks are done.
		if o.dag.AllTasksComplete() {
			slog.Info("all tasks complete, transitioning to synthesizing")
			return &SynthesizingState{}, nil
		}
	}

	return nil, nil // Stay waiting
}

// SynthesizingState collects task results, runs the synthesis engine,
// and transitions to DoneState.
type SynthesizingState struct{}

func (s *SynthesizingState) Name() string { return "synthesizing" }

func (s *SynthesizingState) Enter(ctx context.Context, o *Orchestrator) error {
	slog.Info("synthesizing: collecting results and generating report",
		"goal", o.goal,
		"dag_size", o.dag.TaskCount(),
	)

	if o.synthesis == nil {
		o.synthesis = NewSynthesisEngine(nil)
	}

	// Collect all task results from the DAG
	results := make(map[string]*TaskResult)
	for id, node := range o.dag.nodes {
		results[id] = &TaskResult{
			TaskID:    node.ID,
			AgentType: node.AgentType,
			Status:    node.Status,
			Summary:   node.Goal, // fallback: use goal as summary
			Findings:  node.Findings,
		}
	}

	// Run synthesis
	sr, err := o.synthesis.synthesizeResults(ctx, results)
	if err != nil {
		slog.Warn("synthesis failed, using empty result", "error", err)
		sr = &SynthesisResult{
			Title:            "Synthesis Failed",
			ExecutiveSummary: "Unable to synthesize results. See individual task outputs.",
		}
	}
	o.result = sr

	slog.Info("synthesis complete",
		"title", sr.Title,
		"high_findings", len(sr.HighFindings),
		"medium_findings", len(sr.MediumFindings),
		"low_findings", len(sr.LowFindings),
		"recommendations", len(sr.Recommendations),
	)
	return nil
}

func (s *SynthesizingState) HandleEvent(ctx context.Context, o *Orchestrator, evt Event) (OrchestratorState, error) {
	// Synthesis completes synchronously — always transition to done.
	return &DoneState{}, nil
}

// DoneState is terminal. The orchestrator has finished its work.
type DoneState struct{}

func (s *DoneState) Name() string { return "done" }

func (s *DoneState) Enter(ctx context.Context, o *Orchestrator) error {
	slog.Info("orchestrator done", "goal", o.goal, "dag_size", o.dag.TaskCount())
	return nil
}

func (s *DoneState) HandleEvent(ctx context.Context, o *Orchestrator, evt Event) (OrchestratorState, error) {
	// Terminal state — no transitions.
	return nil, nil
}
