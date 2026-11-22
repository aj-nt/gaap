package gaap

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/aj-nt/gaap/internal/status"
	"github.com/aj-nt/gaap/internal/worker"
	"github.com/aj-nt/gaap/platform"
)

// Getting a task out the door and reading what came back.
//
// Every task leaves through dispatchTask, which is why the breaker check and the
// governor check live inside it rather than in the callers: a path that dispatches
// around them is a path with no governance. Results come back through
// loadWorkerResult, and the simulated run reuses the same promotions.

// dispatchTask sends one task to the daemon and tells the watcher to watch it.
//
// Planning uses it for roots; waiting uses it when a parent's completion promotes
// a child. That second caller is not optional: a promoted task that is never
// dispatched can never run, which is how every multi-level DAG used to sit at
// "ready" until the wait timed out and the run reported failure.
func (o *Orchestrator) dispatchTask(ctx context.Context, taskID string) error {
	node, ok := o.dag.nodes[taskID]
	if !ok {
		return fmt.Errorf("dispatch: unknown task %q", taskID)
	}

	// An open breaker is a stop, and the task stays ready: the run reports it as
	// unfinished rather than pretending it ran.
	if breaker := o.breakerFor(node.AgentType); breaker != nil && !breaker.AllowRequest() {
		slog.Warn("circuit breaker open, skipping dispatch",
			"agent_type", node.AgentType, "task_id", node.ID)
		return nil
	}

	// The governor decides whether this call happens at all. One place, before
	// anything reaches the daemon, so there is no path that dispatches around it.
	// A task a human approved is not asked again: the approval WAS the decision,
	// and asking twice would make "yes" mean nothing.
	if o.cfg.Governor != nil && !node.Approved {
		if err := o.decide(node); err != nil {
			return err
		}
		if node.Status != status.TaskReady {
			// Denied or held for review: the governor already said what happens.
			return nil
		}
	}

	// The context is what the decomposer gave the task, or what a human amended
	// it to with an edit decision. Serialising the node rather than hardcoding
	// "{}" is what makes an edit reach the worker at all.
	ctxJSON := "{}"
	if len(node.Context) > 0 {
		b, err := json.Marshal(node.Context)
		if err != nil {
			return fmt.Errorf("dispatch %s: encoding context: %w", node.ID, err)
		}
		ctxJSON = string(b)
	}
	if _, err := o.daemon.AddTask(ctx, node.ID, node.AgentType, node.Goal, ctxJSON, 3, 300, 2); err != nil {
		slog.Warn("failed to dispatch task", "id", node.ID, "error", err)
		return err
	}

	select {
	case o.dispatchFeed <- node.ID:
	case <-ctx.Done():
	}
	return nil
}

// breakerFor returns the circuit breaker for an agent type, creating it on first
// use. nil means breakers are off. Created lazily because the set of agent types
// is a property of the decomposition, which does not exist when the orchestrator
// is constructed.
func (o *Orchestrator) breakerFor(agentType string) *CircuitBreaker {
	if o.breakerRegistry == nil || agentType == "" {
		return nil
	}
	if cb, ok := o.breakerRegistry[agentType]; ok {
		return cb
	}
	cb := NewCircuitBreaker(agentType, o.cfg.BreakerThreshold,
		time.Duration(o.cfg.BreakerCooldownSec)*time.Second)
	o.breakerRegistry[agentType] = cb
	return cb
}

// recordBreakerFailure opens the agent type's breaker once its dead-letter count
// reaches the threshold. A tripped type is not dispatched to again until its
// cooldown expires.
func (o *Orchestrator) recordBreakerFailure(agentType string) {
	cb := o.breakerFor(agentType)
	if cb == nil {
		return
	}
	cb.RecordFailure()
	if cb.State() == StateOpen {
		slog.Warn("circuit breaker opened, no further dispatch to this agent type",
			"agent_type", agentType, "failures", cb.FailureCount())
	}
}

// recordBreakerSuccess lets a successful probe close a half-open breaker.
func (o *Orchestrator) recordBreakerSuccess(agentType string) {
	if cb := o.breakerFor(agentType); cb != nil {
		cb.RecordSuccess()
	}
}

// decide asks the governor about one task and enacts the answer: allow moves
// nothing (dispatch continues), review queues a question instead of dispatching,
// deny fails the task with the rule's own reason.
//
// The policy is asked about the agent type, because that is the unit the standing
// orders name — "static_analysis may not run here" — and the arguments carry the
// task identity so a rule can be narrower than a type when it needs to be.
func (o *Orchestrator) decide(node *TaskNode) error {
	agentID := o.cfg.AgentID
	if agentID == "" {
		agentID = "gaap-orchestrator"
	}
	d := o.cfg.Governor.Decide(agentID, node.AgentType, map[string]any{
		"task_id": node.ID,
		"goal":    node.Goal,
		"repo":    o.cfg.RepoPath,
		"context": node.Context,
	}, platform.WithRun(o.RunID()), platform.WithParents(node.ParentIDs...))
	slog.Info("govern: decision",
		"task_id", node.ID,
		"agent_type", node.AgentType,
		"action", string(d.Action),
		"rule", d.RuleID,
		"reason", d.Reason)

	switch d.Action {
	case platform.ActionAllow:
		return nil
	case platform.ActionReview:
		if !o.cfg.Approval.Enabled {
			// A review nobody can answer is a hang. Without the approval loop the
			// safe reading of "a person should look at this" is that it does not
			// run.
			o.denyTaskPolicy(node, d, "policy asked for review and no approval loop is configured: "+d.Reason)
			return nil
		}
		node.Status = status.TaskBlocked
		o.pendingReviews = append(o.pendingReviews, reviewRecord{
			TaskID:    node.ID,
			AgentType: node.AgentType,
			Reason:    ReviewReason(d.Reason),
		})
		return nil
	default: // deny, and anything the kernel might add later
		o.denyTaskPolicy(node, d, d.Reason)
		return nil
	}
}

// denyTask fails a task the policy refused. It does not queue a question: a deny
// is a decision, not a request for one, and asking a human to overrule the
// standing orders would make the policy advisory.
func (o *Orchestrator) denyTask(node *TaskNode, reason string) {
	o.failDenied(node, "policy deny: "+reason, "")
}

// denyTaskPolicy is denyTask for a refusal that came with a structured Denial,
// and it also writes the text the agent will read.
//
// Distinct from denyTask because the authorship differs. This refusal is the
// platform's own standing orders, so the reframe says so and stops an agent
// reading it as a target's defence to route around. A human declining a task
// (approval.go) must NOT be reframed that way: telling an agent "your platform
// blocked this" when a person did sends it looking for a policy to satisfy.
func (o *Orchestrator) denyTaskPolicy(node *TaskNode, d platform.Decision, reason string) {
	model := ""
	if d.Denial != nil {
		model = platform.ReframeDenial(*d.Denial)
	}
	o.failDenied(node, "policy deny: "+reason, model)
}

// failDenied is the shared tail: the task fails, its descendants are skipped, and
// the failure is emitted. Summary stays the raw text because it is evidence; the
// reframe goes alongside it, never in place of it.
func (o *Orchestrator) failDenied(node *TaskNode, summary, summaryModel string) {
	node.Status = status.TaskFailed
	node.Summary = summary
	if summaryModel != "" {
		node.SummaryModel = summaryModel
	}
	skipped := o.dag.SkipDescendants(node.ID)
	o.fireTaskFailed(node.ID, "govern", skipped)
}

// loadWorkerResult reads the ExecuteResult a worker stored under the task's
// ResultKey. A dead-lettered task has no result key at all, which is the point:
// its summary comes from failTask, never from a missing entry read as empty.
func (o *Orchestrator) loadWorkerResult(resultKey string, node *TaskNode) {
	if resultKey == "" {
		return
	}
	mem, err := o.daemon.GetMemory(o.ctx, resultKey)
	if err != nil || mem == nil {
		return
	}
	var wr worker.ExecuteResult
	if json.Unmarshal([]byte(mem.Content), &wr) != nil {
		return
	}
	node.Findings = wr.Findings
	node.Summary = wr.Summary
}

// autoCompleteSynthesis marks synthesis tasks promoted to ready as done — they
// run in-process, so there is no daemon task to poll.
func (o *Orchestrator) autoCompleteSynthesis(logPrefix string) {
	for _, n := range o.dag.nodes {
		if n.AgentType == "synthesis" && n.Status == status.TaskReady {
			n.Status = status.TaskDone
			o.fireTaskCompleted(n.ID, logPrefix)
		}
	}
}

// simulateCompletions marks all ready/leaf tasks as done and advances the DAG.
// Used by tests and when the daemon is unavailable (NullMnemo).
func (o *Orchestrator) simulateCompletions() {
	// Completions are posted, not applied inline, so this loop cannot rely on a
	// handler promoting children between passes: it promotes them itself and
	// repeats until the DAG stops moving.
	for {
		progress := false
		for id, node := range o.dag.nodes {
			if node.Status == status.TaskReady {
				node.Status = status.TaskDone
				o.fireTaskCompleted(id, "sim")
				progress = true
			}
		}
		for _, n := range o.dag.nodes {
			if n.Status != status.TaskBlocked {
				continue
			}
			if allDone, err := o.dag.AllParentsComplete(n.ID); err == nil && allDone {
				if err := o.dag.PromoteToReady(n.ID); err == nil {
					progress = true
				}
			}
		}
		if !progress {
			return
		}
	}
}
