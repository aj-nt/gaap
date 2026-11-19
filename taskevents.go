package gaap

import (
	"log/slog"

	"github.com/aj-nt/gaap/internal/status"
	"github.com/aj-nt/vassago-sdk/client"
	pb "github.com/aj-nt/vassago-sdk/proto"
)

// The two transports, reduced to one vocabulary.
//
// The poll path speaks client.TaskEntry and the subscription path proto.TaskEntry;
// they are different SDK structs, and mapping both separately is how dead_letter
// came to be recorded as done on one path and not the other. taskEvent is the
// only shape anything downstream reads, so the mapping cannot drift.

// taskEvent is the subset of a daemon task entry either transport can supply:
// the poll path speaks client.TaskEntry, the subscription path proto.TaskEntry,
// and those are different structs in the SDK. Reducing both to these four
// fields is what keeps one mapping instead of two.
type taskEvent struct {
	ID         string
	Status     status.Task
	ResultKey  string
	RetryCount int32
	Source     string // "poll" or "sub", for logging only
}

// Terminal reports whether this status means nothing further will happen to the
// task, so the watcher can stop polling it.
func (e taskEvent) Terminal() bool {
	switch e.Status {
	case status.TaskDone, status.TaskFailed, status.TaskDeadLetter, status.TaskSkipped:
		return true
	}
	return false
}

// taskEventFromClient and taskEventFromProto reduce the two SDK entry shapes to
// the one vocabulary the DAG speaks, so the mapping is testable without a daemon
// and cannot drift between transports.
func taskEventFromClient(e *client.TaskEntry) taskEvent {
	return taskEvent{
		ID:         e.Id,
		Status:     status.Task(e.Status),
		ResultKey:  e.ResultKey,
		RetryCount: e.RetryCount,
		Source:     "poll",
	}
}

func taskEventFromProto(t *pb.TaskEntry) taskEvent {
	return taskEvent{
		ID:         t.Id,
		Status:     status.Task(t.Status),
		ResultKey:  t.ResultKey,
		RetryCount: t.RetryCount,
		Source:     "sub",
	}
}

// handleTaskEvent advances the DAG for one daemon task event. Both transport
// paths call it, so the mapping from daemon vocabulary to DAG vocabulary lives
// in one place and cannot drift between them.
func (o *Orchestrator) handleTaskEvent(evt taskEvent) {
	node, ok := o.dag.nodes[evt.ID]
	if !ok {
		return // not one of our tasks
	}
	oldStatus := node.Status
	switch evt.Status {
	case status.TaskDone:
		node.Status = status.TaskDone
		o.loadWorkerResult(evt.ResultKey, node)
		slog.Info(evt.Source+": task completed",
			"id", evt.ID, "old_status", oldStatus, "new_status", node.Status)
		o.recordBreakerSuccess(node.AgentType)
		o.fireTaskCompleted(evt.ID, evt.Source)
	case status.TaskDeadLetter:
		// The breaker counts terminal failures, not retries: a task that failed
		// twice and then succeeded is not evidence the agent type is broken.
		o.recordBreakerFailure(node.AgentType)
		o.failTask(node, evt.RetryCount, evt.Source)
	}
}

// fireTaskCompleted and fireTaskFailed post the two task events the state machine
// answers. They post rather than call HandleEvent directly: the loop is the only
// caller, so ordering stays FIFO and no handler runs inside another handler.
func (o *Orchestrator) fireTaskCompleted(taskID, logPrefix string) {
	slog.Info(logPrefix+": task completed", "id", taskID)
	o.post(Event{
		Type:    EventTaskCompleted,
		Payload: map[string]any{"task_id": taskID},
	})
}

func (o *Orchestrator) fireTaskFailed(taskID, logPrefix string, skipped []string) {
	slog.Warn(logPrefix+": task failed", "id", taskID, "skipped", skipped)
	o.post(Event{
		Type:    EventTaskFailed,
		Payload: map[string]any{"task_id": taskID, "skipped": skipped},
	})
}
