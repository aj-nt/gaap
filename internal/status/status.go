// Package status holds the task vocabulary shared by the orchestrator (task
// nodes), the worker pool (claim checks) and the worker executor (results).
//
// These strings are the wire vocabulary of the Vassago daemon's task store —
// the daemon enforces them with a CHECK constraint — so they are not
// free-form. Keeping them in one place turns a misspelled state from a
// silent runtime no-op into a compile error.
package status

// Task is a task's lifecycle state, in the daemon and in the orchestrator's DAG.
// Mirrors vassago's daemon/internal/store.TaskStatus.
type Task string

const (
	TaskBlocked    Task = "blocked"
	TaskReady      Task = "ready"
	TaskClaimed    Task = "claimed"
	TaskDone       Task = "done"
	TaskFailed     Task = "failed"
	TaskDeadLetter Task = "dead_letter"

	// TaskSkipped is DAG-local: a descendant of a failed task that was never
	// dispatched and never will be. It has no daemon equivalent — no task row
	// is ever skipped — so it must be normalized before being persisted into a
	// RunState or written to the store.
	TaskSkipped Task = "skipped"
)

// Result is a worker's execution outcome, recorded inside ExecuteResult.
// Deliberately a different type from Task: a task reaches TaskDone when a
// worker reports ResultSuccess, and the two words never travel the same
// channel.
type Result string

const (
	ResultSuccess Result = "success"
	ResultFailed  Result = "failed"
)
