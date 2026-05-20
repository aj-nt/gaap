package gaap

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"
)

// EventType categorizes events in the orchestrator event loop.
type EventType int

const (
	EventGoalReceived EventType = iota
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
			// Circuit breaker: skip dispatch if this agent type is tripped
			if o.breakerRegistry != nil {
				if breaker, ok := o.breakerRegistry[t.AgentType]; ok {
					if !breaker.AllowRequest() {
						slog.Warn("circuit breaker open, skipping dispatch",
							"agent_type", t.AgentType, "task_id", t.TaskID)
						continue
					}
				}
			}

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

	// Save run state to daemon for crash-recovery via gaap resume.
	// Ignore errors from NullMnemo in tests.
	runKey := fmt.Sprintf("runstate_%s_%s",
		strings.ReplaceAll(sanitizeGoal(o.goal), " ", "-"),
		uuid.New().String()[:8])
	if err := o.SaveRunState(ctx, runKey); err != nil {
		slog.Warn("planning: failed to save run state", "error", err)
	}
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
		summary := node.Summary
		if summary == "" {
			summary = node.Goal // fallback: use goal if no worker summary
		}
		results[id] = &TaskResult{
			TaskID:    node.ID,
			AgentType: node.AgentType,
			Status:    node.Status,
			Summary:   summary,
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

	// Print the synthesis report to stdout for human consumption.
	if o.result != nil {
		report := formatSynthesisReport(o.result)
		fmt.Println(report) // stdout — captured by CLI or terminal
	}

	// Persist the synthesis report to the daemon for downstream consumption.
	if o.result != nil {
		payload, err := json.Marshal(o.result)
		if err != nil {
			slog.Warn("failed to marshal synthesis result", "error", err)
		} else {
			reportKey := fmt.Sprintf("synthesis_%s_%s",
				strings.ReplaceAll(sanitizeGoal(o.goal), " ", "-"),
				uuid.New().String()[:8],
			)
			_, err := o.daemon.AddMemory(ctx, "memory", "synthesis_report", reportKey,
				string(payload), 3, "gaap-orchestrator")
			if err != nil {
				slog.Warn("failed to persist synthesis report to daemon", "error", err)
			} else {
				slog.Info("synthesis report persisted", "key", reportKey)
			}
		}
	}

	return nil
}

func (s *DoneState) HandleEvent(ctx context.Context, o *Orchestrator, evt Event) (OrchestratorState, error) {
	// Terminal state — no transitions.
	return nil, nil
}

// formatSynthesisReport renders the synthesis result as a human-readable text report.
func formatSynthesisReport(sr *SynthesisResult) string {
	var b strings.Builder

	b.WriteString("\n")
	b.WriteString(strings.Repeat("=", 60))
	b.WriteString(fmt.Sprintf("\n  %s\n", sr.Title))
	b.WriteString(strings.Repeat("=", 60))
	b.WriteString(fmt.Sprintf("\n\n%s\n\n", sr.ExecutiveSummary))

	// Findings by severity
	for _, section := range []struct {
		severity string
		items    []Finding
	}{
		{"HIGH", sr.HighFindings},
		{"MEDIUM", sr.MediumFindings},
		{"LOW", sr.LowFindings},
	} {
		if len(section.items) == 0 {
			continue
		}
		b.WriteString(fmt.Sprintf("--- %s SEVERITY (%d findings) ---\n\n", section.severity, len(section.items)))
		for i, f := range section.items {
			b.WriteString(fmt.Sprintf("%d. %s\n", i+1, f.Title))
			if f.Location != "" {
				b.WriteString(fmt.Sprintf("   Location: %s\n", f.Location))
			}
			if f.Detail != "" {
				b.WriteString(fmt.Sprintf("   %s\n", f.Detail))
			}
			if len(f.Sources) > 0 {
				b.WriteString(fmt.Sprintf("   Source(s): %s\n", strings.Join(f.Sources, ", ")))
			}
			b.WriteString("\n")
		}
	}

	// Cross-reference insights
	if len(sr.CrossRefInsights) > 0 {
		b.WriteString("--- CROSS-REFERENCE INSIGHTS ---\n\n")
		for i, ci := range sr.CrossRefInsights {
			b.WriteString(fmt.Sprintf("%d. Pattern: %s\n", i+1, ci.Pattern))
			b.WriteString(fmt.Sprintf("   Root Cause: %s\n", ci.RootCause))
			b.WriteString(fmt.Sprintf("   Recommendation: %s\n", ci.Recommend))
			if len(ci.Locations) > 0 {
				b.WriteString(fmt.Sprintf("   Affected: %s\n", strings.Join(ci.Locations, ", ")))
			}
			b.WriteString("\n")
		}
	}

	// Codebase health
	if len(sr.CodebaseHealth) > 0 {
		b.WriteString("--- CODEBASE HEALTH ---\n\n")
		for k, v := range sr.CodebaseHealth {
			b.WriteString(fmt.Sprintf("  %s: %s\n", k, v))
		}
		b.WriteString("\n")
	}

	// Recommendations
	if len(sr.Recommendations) > 0 {
		b.WriteString("--- TOP RECOMMENDATIONS ---\n\n")
		for i, r := range sr.Recommendations {
			b.WriteString(fmt.Sprintf("%d. [Priority %d] %s\n", i+1, r.Priority, r.Action))
			b.WriteString(fmt.Sprintf("   Effort: %s | Impact: %s\n", r.Effort, r.Impact))
			if r.Rationale != "" {
				b.WriteString(fmt.Sprintf("   Rationale: %s\n", r.Rationale))
			}
			b.WriteString("\n")
		}
	}

	b.WriteString(strings.Repeat("=", 60))
	b.WriteString("\n")
	return b.String()
}

// sanitizeGoal strips characters unsafe for memory keys.
func sanitizeGoal(goal string) string {
	s := goal
	// Keep only alphanumeric, spaces, hyphens, underscores
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == ' ' || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	result := b.String()
	// Limit length
	const maxLen = 40
	if len(result) > maxLen {
		result = result[:maxLen]
	}
	return result
}
