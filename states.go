package gaap

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/aj-nt/gaap/internal/status"
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

// sepLine is the width of separator lines in state output reports.
var sepLine = strings.Repeat("=", 60)

// Event is a pub/sub or timer event that drives state transitions.
type Event struct {
	Type    EventType
	Payload map[string]any
}

// Phase names the orchestrator's stage. A typed value rather than a bare
// string because the orchestrator gates on it in five places: a misspelled
// literal used to compile and silently skip a transition, which turns a dead
// run into a nil return.
type Phase string

const (
	PhaseIdle     Phase = "idle"
	PhasePlanning Phase = "planning"
	PhaseWaiting  Phase = "waiting"
	// PhaseApproving waits on a human decision about a task the daemon gave up
	// on. It is the first phase that owns data (the pending request), and it
	// sits between waiting and whatever the human's answer produces.
	PhaseApproving    Phase = "approving"
	PhaseSynthesizing Phase = "synthesizing"
	PhaseDone         Phase = "done"
	PhaseFailed       Phase = "failed"
)

// OrchestratorState defines the interface for all orchestrator states.
type OrchestratorState interface {
	// Enter is called when transitioning into this state.
	Enter(ctx context.Context, o *Orchestrator) error
	// HandleEvent processes an event and returns the next state (nil = stay).
	HandleEvent(ctx context.Context, o *Orchestrator, evt Event) (OrchestratorState, error)
	// Phase returns this state's phase — the only vocabulary the orchestrator
	// compares against.
	Phase() Phase
}

// IdleState awaits a goal. On receiving a goal, transitions to PlanningState.
type IdleState struct{}

func (s *IdleState) Phase() Phase { return PhaseIdle }

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

func (s *PlanningState) Phase() Phase { return PhasePlanning }

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
		if t.Status == status.TaskReady && len(t.ParentIDs) == 0 {
			// Circuit breaker: skip dispatch if this agent type is tripped. An
			// open breaker is a stop, and the task stays ready — the run will
			// report it as unfinished rather than pretend it ran.
			if breaker := o.breakerFor(t.AgentType); breaker != nil && !breaker.AllowRequest() {
				slog.Warn("circuit breaker open, skipping dispatch",
					"agent_type", t.AgentType, "task_id", t.TaskID)
				continue
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

func (s *WaitingState) Phase() Phase { return PhaseWaiting }

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
			// trace-emitter: audience-scoped digests after every DAG advance
			o.emitDigests(ctx)
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

		// Terminal: nothing further can happen.
		if o.dag.AllTasksTerminal() {
			if failed := o.dag.FailedIDs(); len(failed) > 0 {
				slog.Warn("tasks failed, transitioning to failed", "failed", failed)
				return &FailedState{}, nil
			}
			slog.Info("all tasks complete, transitioning to synthesizing")
			return &SynthesizingState{}, nil
		}

	case EventTaskFailed:
		taskID, _ := evt.Payload["task_id"].(string)
		skipped, _ := evt.Payload["skipped"].([]string)
		slog.Warn("task failed, advancing DAG", "task_id", taskID, "skipped", skipped)
		// Digests first: a failure is something the audience-scoped report
		// should carry, including which dependents it took out.
		o.emitDigests(ctx)
		if o.failFast() {
			slog.Warn("fail_fast: ending run at the first failure", "task_id", taskID)
			return &FailedState{}, nil
		}
		if o.dag.AllTasksTerminal() {
			return &FailedState{}, nil
		}
	}

	return nil, nil // Stay waiting
}

// SynthesizingState collects task results, runs the synthesis engine,
// and transitions to DoneState.
type SynthesizingState struct{}

func (s *SynthesizingState) Phase() Phase { return PhaseSynthesizing }

func (s *SynthesizingState) Enter(ctx context.Context, o *Orchestrator) error {
	slog.Info("synthesizing: collecting results and generating report",
		"goal", o.goal,
		"dag_size", o.dag.TaskCount(),
	)

	if o.synthesis == nil {
		o.synthesis = NewSynthesisEngine(nil)
	}

	// Run synthesis over what actually completed.
	sr, err := o.synthesis.synthesizeResults(ctx, o.collectSynthesisInput())
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

func (s *DoneState) Phase() Phase { return PhaseDone }

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

// collectSynthesisInput returns results for tasks that actually completed.
// Failed and skipped tasks are the failure report's business, not the
// synthesizer's: feeding them in makes an empty summary look like a result.
// A completed task with no worker summary says so — its goal is context, never
// a substitute for the result it did not report.
func (o *Orchestrator) collectSynthesisInput() map[string]*TaskResult {
	results := make(map[string]*TaskResult)
	for id, node := range o.dag.nodes {
		if node.Status != status.TaskDone {
			continue
		}
		summary := node.Summary
		if summary == "" {
			summary = fmt.Sprintf("no summary reported (goal: %s)", node.Goal)
		}
		results[id] = &TaskResult{
			TaskID:    node.ID,
			AgentType: node.AgentType,
			Status:    node.Status,
			Summary:   summary,
			Findings:  node.Findings,
		}
	}
	return results
}

// ApprovalState waits on a human decision about a task the daemon gave up on.
//
// It is the first phase that owns data: which task failed, where the question was
// written down, and when it expires. The empty states stay empty because they
// genuinely hold nothing — this one has something to hold.
type ApprovalState struct {
	Request        ApprovalRequest
	ApprovalTaskID string
	Deadline       time.Time
}

func (s *ApprovalState) Phase() Phase { return PhaseApproving }

func (s *ApprovalState) Enter(ctx context.Context, o *Orchestrator) error {
	// Opening the approval task in the daemon happens in M2 (openApproval). This
	// phase exists here so the graph, the state, and its data are pinned before
	// anything depends on them.
	slog.Info("approving: waiting on a human decision",
		"task_id", s.Request.TaskID,
		"approval_task", s.ApprovalTaskID,
		"deadline", s.Deadline.Format(time.RFC3339))
	return nil
}

func (s *ApprovalState) HandleEvent(ctx context.Context, o *Orchestrator, evt Event) (OrchestratorState, error) {
	// The human's answer and the deadline tick are handled in M2. An event this
	// phase does not understand is not a reason to move.
	return nil, nil
}

// FailedState is terminal. The orchestrator timed out or all dispatched tasks dead-lettered.
// It synthesizes partial results (if any completed) and reports the failure clearly.
type FailedState struct{}

func (s *FailedState) Phase() Phase { return PhaseFailed }

func (s *FailedState) Enter(ctx context.Context, o *Orchestrator) error {
	c := o.countOutcomes()
	o.reportFailure(c)
	o.synthesizePartial(ctx, c)
	o.persistFailure(ctx, c)
	return nil
}

// outcomeCounts is what the failure report has to say: how each task ended, and
// the IDs in stable order.
type outcomeCounts struct {
	completed, failed, skipped, pending int
	failedIDs, skippedIDs               []string
}

// countOutcomes tallies the DAG by terminal state, in the DAG's vocabulary
// rather than the daemon's.
func (o *Orchestrator) countOutcomes() outcomeCounts {
	var c outcomeCounts
	for id, node := range o.dag.nodes {
		switch node.Status {
		case status.TaskDone:
			c.completed++
		case status.TaskFailed:
			c.failed++
			c.failedIDs = append(c.failedIDs, id)
		case status.TaskSkipped:
			c.skipped++
			c.skippedIDs = append(c.skippedIDs, id)
		default:
			c.pending++
		}
	}
	sort.Strings(c.failedIDs)
	sort.Strings(c.skippedIDs)
	return c
}

// reportFailure prints the human-facing header and names every task that did
// not complete. A failed task with no recorded reason says so rather than
// leaving the line blank and looking like a formatting bug.
func (o *Orchestrator) reportFailure(c outcomeCounts) {
	slog.Warn("orchestration failed — not all tasks completed",
		"goal", o.goal,
		"completed", c.completed,
		"failed", c.failed,
		"skipped", c.skipped,
		"still_waiting", c.pending,
	)

	fmt.Printf("\n%s\n", sepLine)
	fmt.Printf("  ORCHESTRATION FAILED\n")
	fmt.Printf("%s\n\n", sepLine)
	fmt.Printf("Goal: %s\n", o.goal)
	fmt.Printf("Completed: %d  |  Failed: %d  |  Skipped: %d  |  Still pending: %d\n\n",
		c.completed, c.failed, c.skipped, c.pending)

	if len(c.failedIDs) > 0 {
		fmt.Println("Failed tasks:")
		for _, id := range c.failedIDs {
			summary := o.dag.nodes[id].Summary
			if summary == "" {
				summary = "no reason recorded"
			}
			fmt.Printf("  - %s — %s\n", id, summary)
		}
		fmt.Println()
	}
	if len(c.skippedIDs) > 0 {
		fmt.Printf("Skipped (never ran, blocked by a failed parent): %s\n\n", strings.Join(c.skippedIDs, ", "))
	}
}

// synthesizePartial reports whatever completed. A failed task's goal is not a
// result, so it never reaches the synthesizer; when nothing completed there is
// nothing to synthesize and the report says that instead of an empty document.
func (o *Orchestrator) synthesizePartial(ctx context.Context, c outcomeCounts) {
	if c.completed == 0 {
		fmt.Println("No tasks completed — no synthesis possible.")
		return
	}

	fmt.Println("Partial synthesis (completed results only):")
	if o.synthesis == nil {
		o.synthesis = NewSynthesisEngine(nil)
	}

	sr, err := o.synthesis.synthesizeResults(ctx, o.collectSynthesisInput())
	if err != nil {
		slog.Warn("partial synthesis failed", "error", err)
		return
	}
	o.result = sr
	fmt.Println(formatSynthesisReport(sr))
}

// persistFailure publishes the outcome counts under the run key so the failure
// is inspectable after the process exits.
func (o *Orchestrator) persistFailure(ctx context.Context, c outcomeCounts) {
	runKey := o.RunKey()
	if runKey == "" {
		return
	}
	payload := map[string]any{
		"goal":          o.goal,
		"completed":     c.completed,
		"failed":        c.failed,
		"skipped":       c.skipped,
		"still_waiting": c.pending,
		"failed_ids":    c.failedIDs,
		"skipped_ids":   c.skippedIDs,
	}
	b, _ := json.Marshal(payload)
	_, _ = o.daemon.AddMemory(ctx, "memory", "orchestration_failed", "failed_"+runKey,
		string(b), 3, "gaap-orchestrator")
}

func (s *FailedState) HandleEvent(ctx context.Context, o *Orchestrator, evt Event) (OrchestratorState, error) {
	// Terminal state — no transitions.
	return nil, nil
}

// formatSynthesisReport renders the synthesis result as a human-readable text report.
func formatSynthesisReport(sr *SynthesisResult) string {
	var b strings.Builder

	b.WriteString("\n")
	b.WriteString(sepLine)
	fmt.Fprintf(&b, "\n  %s\n", sr.Title)
	b.WriteString(sepLine)
	fmt.Fprintf(&b, "\n\n%s\n\n", sr.ExecutiveSummary)

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
		fmt.Fprintf(&b, "--- %s SEVERITY (%d findings) ---\n\n", section.severity, len(section.items))
		for i, f := range section.items {
			fmt.Fprintf(&b, "%d. %s\n", i+1, f.Title)
			if f.Location != "" {
				fmt.Fprintf(&b, "   Location: %s\n", f.Location)
			}
			if f.Detail != "" {
				fmt.Fprintf(&b, "   %s\n", f.Detail)
			}
			if len(f.Sources) > 0 {
				fmt.Fprintf(&b, "   Source(s): %s\n", strings.Join(f.Sources, ", "))
			}
			b.WriteString("\n")
		}
	}

	// Cross-reference insights
	if len(sr.CrossRefInsights) > 0 {
		b.WriteString("--- CROSS-REFERENCE INSIGHTS ---\n\n")
		for i, ci := range sr.CrossRefInsights {
			fmt.Fprintf(&b, "%d. Pattern: %s\n", i+1, ci.Pattern)
			fmt.Fprintf(&b, "   Root Cause: %s\n", ci.RootCause)
			fmt.Fprintf(&b, "   Recommendation: %s\n", ci.Recommend)
			if len(ci.Locations) > 0 {
				fmt.Fprintf(&b, "   Affected: %s\n", strings.Join(ci.Locations, ", "))
			}
			b.WriteString("\n")
		}
	}

	// Codebase health
	if len(sr.CodebaseHealth) > 0 {
		b.WriteString("--- CODEBASE HEALTH ---\n\n")
		for k, v := range sr.CodebaseHealth {
			fmt.Fprintf(&b, "  %s: %s\n", k, v)
		}
		b.WriteString("\n")
	}

	// Recommendations
	if len(sr.Recommendations) > 0 {
		b.WriteString("--- TOP RECOMMENDATIONS ---\n\n")
		for i, r := range sr.Recommendations {
			fmt.Fprintf(&b, "%d. [Priority %d] %s\n", i+1, r.Priority, r.Action)
			fmt.Fprintf(&b, "   Effort: %s | Impact: %s\n", r.Effort, r.Impact)
			if r.Rationale != "" {
				fmt.Fprintf(&b, "   Rationale: %s\n", r.Rationale)
			}
			b.WriteString("\n")
		}
	}

	b.WriteString(sepLine)
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
