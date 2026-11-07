package gaap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/aj-nt/gaap/internal/status"
	"github.com/aj-nt/gaap/internal/worker"
	"github.com/aj-nt/vassago-sdk/client"
	pb "github.com/aj-nt/vassago-sdk/proto"
)

// MnemoClient is the Vassago daemon client interface the orchestrator talks to.
// Re-exported from the SDK so the orchestrator's signatures stay readable.
type MnemoClient = client.MnemoClient

// FailureMode is how a run reacts to a task failure.
type FailureMode string

const (
	// FailSoft lets independent branches finish, then reports the failure.
	FailSoft FailureMode = "fail_soft"
	// FailFast ends the run as soon as any task fails.
	FailFast FailureMode = "fail_fast"
)

// Mode returns the configured failure mode. The zero value and any unrecognized
// string mean FailSoft: the safe default is to finish work already in flight
// rather than to throw it away.
func (c *Config) Mode() FailureMode {
	if c.FailureMode == FailFast {
		return FailFast
	}
	return FailSoft
}

// Config holds the orchestrator configuration.
type Config struct {
	DaemonAddr      string `yaml:"daemon_addr"`
	RepoPath        string `yaml:"repo_path"`
	MaxWaitSec      int    `yaml:"max_wait_sec"`
	PollIntervalSec int    `yaml:"poll_interval_sec"`

	// FailureMode decides what one failed task does to the run.
	FailureMode FailureMode `yaml:"failure_mode"`

	// BreakerThreshold is how many dead-lettered tasks for one agent type, inside
	// the cooldown window, open that type's circuit breaker. Zero disables
	// breakers entirely (the default): a breaker stops dispatch to an agent type
	// mid-run, which should be a decision an operator makes, not a surprise.
	BreakerThreshold int `yaml:"breaker_threshold"`
	// BreakerCooldownSec is how long an open breaker refuses dispatch, and the
	// window failures are counted in. Defaults to 60 when breakers are on.
	BreakerCooldownSec int `yaml:"breaker_cooldown_sec"`
}

// ErrOrchestrationFailed is returned when a run finishes with at least one
// failed task. The report is still produced and persisted — the error is what
// makes the process exit non-zero instead of reporting success over a hole.
var ErrOrchestrationFailed = errors.New("orchestration failed: one or more tasks did not complete")

// RunState is a serializable snapshot of an orchestrator run that can be
// stored to and loaded from the daemon. It captures just enough to rebuild
// the DAG and resume from the Waiting state after an orchestrator crash.
type RunState struct {
	Goal        string      `json:"goal"`
	RepoPath    string      `json:"repo_path"`
	Tasks       []TaskSpec  `json:"tasks"`
	FailureMode FailureMode `json:"failure_mode,omitempty"`
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
	runKey          string

	// subscribeFallbackToPoll controls the observer pattern: when true,
	// subscribeDaemon() attempts gRPC subscription first, falling back to
	// polling on failure. False (default) uses polling only (backward compat).
	subscribeFallbackToPoll bool // daemon key for persisted RunState

	// digestMaxChars is the per-audience digest budget (default
	// defaultDigestMaxChars). A field, not a package global, so tests can
	// override it without racing parallel readers.
	digestMaxChars int
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
		cfg:            cfg,
		ctx:            ctx,
		daemon:         daemon,
		decomposer:     decomposer,
		synthesis:      NewSynthesisEngine(nil), // schema-only by default; set later for LLM
		dag:            NewDAG(),
		state:          &IdleState{},
		digestMaxChars: defaultDigestMaxChars,
	}
	if o.cfg.BreakerThreshold > 0 {
		if o.cfg.BreakerCooldownSec <= 0 {
			o.cfg.BreakerCooldownSec = 60
		}
		o.breakerRegistry = make(map[string]*CircuitBreaker)
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

// SetSubscribeFallbackToPoll enables or disables the observer pattern (push-based
// task status updates via gRPC subscription). When enabled, the orchestrator attempts
// subscription first and falls back to polling on failure. Defaults to false (polling only).
func (o *Orchestrator) SetSubscribeFallbackToPoll(enabled bool) {
	o.subscribeFallbackToPoll = enabled
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
	if o.state.Phase() == PhasePlanning {
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

// resumeFromState rebuilds the DAG from a saved RunState, translating any
// persisted status into the DAG's vocabulary. A RunState written by an older
// build can carry the daemon's dead_letter, which the DAG does not speak.
func (o *Orchestrator) resumeFromState(rs *RunState) error {
	// Restore the failure policy the run started with. A RunState saved before
	// this field existed has none, which normalizes to fail_soft.
	if rs.FailureMode != "" {
		o.cfg.FailureMode = rs.FailureMode
	}

	o.dag = NewDAG()
	for _, t := range rs.Tasks {
		st := t.Status
		if st == status.TaskDeadLetter {
			st = status.TaskFailed
		}
		err := o.dag.AddTask(&TaskNode{
			ID:        t.TaskID,
			ParentIDs: t.ParentIDs,
			Status:    st,
			Goal:      t.Goal,
			AgentType: t.AgentType,
			Context:   t.Context,
		})
		if err != nil {
			return fmt.Errorf("resume: add task %s: %w", t.TaskID, err)
		}
	}
	return nil
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

	if err := o.resumeFromState(rs); err != nil {
		return err
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
	if o.state.Phase() == PhaseWaiting {
		// Start workers if configured
		var workerStopCh chan struct{}
		if o.workerPool != nil {
			workerStopCh = make(chan struct{})
			go o.workerPool.Run(o.ctx, workerStopCh)
			slog.Info("auto-workers started")
		}

		// Observer pattern: try gRPC subscription first, fall back to polling.
		if o.subscribeFallbackToPoll {
			if err := o.subscribeDaemon(); err != nil {
				slog.Info("subscription failed, falling back to polling", "error", err)
				o.pollDaemon()
			}
		} else {
			o.pollDaemon()
		}

		// Timed out with work still pending — transition to Failed.
		if !o.dag.AllTasksComplete() && o.state.Phase() == PhaseWaiting {
			slog.Warn("orchestration incomplete after wait phase, transitioning to failed")
			if err := o.Transition(&FailedState{}); err != nil {
				return fmt.Errorf("failed transition: %w", err)
			}
		}

		// Stop workers
		if workerStopCh != nil {
			close(workerStopCh)
			slog.Info("auto-workers stopped")
		}
	}

	// Synthesizing completes synchronously and transitions to Done
	if o.state.Phase() == PhaseSynthesizing {
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

	// A run that ended in FailedState produced a report, but it did not
	// succeed: say so, so the process exits non-zero.
	if o.state.Phase() == PhaseFailed {
		return ErrOrchestrationFailed
	}

	return nil
}

// SaveRunState persists the orchestrator's current DAG as a RunState to the
// daemon. This allows crash recovery via Resume(). The returned key is the
// daemon memory key that can be passed to gaap resume.
func (o *Orchestrator) SaveRunState(ctx context.Context, runKey string) error {
	rs := &RunState{
		Goal:        o.goal,
		RepoPath:    o.cfg.RepoPath,
		FailureMode: o.cfg.Mode(),
		Tasks:       make([]TaskSpec, 0, len(o.dag.nodes)),
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

		// Stop when nothing further can happen. AllTasksTerminal covers the
		// failed/skipped case that AllTasksComplete treats as "still running",
		// which is how a dead-lettered task used to burn the whole maxWait.
		if o.dag.AllTasksTerminal() {
			return
		}
		// Fail-fast stops even when other branches are still in flight.
		if o.failFast() && o.dag.HasFailures() {
			slog.Warn("polling: stopping at first failure (fail_fast)")
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
		if node.Status == status.TaskReady || node.Status == status.TaskClaimed {
			entry, err := o.daemon.GetTask(o.ctx, node.ID)
			if err != nil {
				slog.Warn("poll: failed to get task", "id", node.ID, "error", err)
				continue
			}
			if entry == nil {
				continue
			}
			o.handleTaskEvent(taskEvent{
				ID:         entry.Id,
				Status:     status.Task(entry.Status),
				ResultKey:  entry.ResultKey,
				RetryCount: entry.RetryCount,
				Source:     "poll",
			})
		}
	}

	// Auto-complete synthesis tasks that were promoted to ready.
	// Synthesis runs in-process after all leaves complete, so there's no
	// daemon task to poll -- we mark them done and fire completion events
	// so the state machine can advance.
	o.autoCompleteSynthesis("poll:synthesis")
}

// fireTaskCompleted fires a task completion event through the state machine.
// Extracted from pollOnce, simulateCompletions, and subscribeDaemon.
func (o *Orchestrator) fireTaskCompleted(taskID, logPrefix string) {
	slog.Info(logPrefix+": task completed", "id", taskID)
	evt := Event{
		Type:    EventTaskCompleted,
		Payload: map[string]any{"task_id": taskID},
	}
	next, err := o.state.HandleEvent(o.ctx, o, evt)
	if err != nil {
		slog.Warn("completion handler error", "task_id", taskID, "error", err)
	}
	if next != nil {
		if err := o.Transition(next); err != nil {
			slog.Warn("state transition failed", "error", err)
		}
	}
}

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

// failTask records a task the daemon gave up on. dead_letter is the daemon's
// word for "retries exhausted"; failed is the DAG's word for what that means.
// Descendants are skipped here because nothing will ever promote them, and
// leaving them blocked would end the run by timeout instead of by fact.
func (o *Orchestrator) failTask(node *TaskNode, attempts int32, logPrefix string) {
	node.Status = status.TaskFailed
	node.Summary = fmt.Sprintf("daemon dead-lettered the task after %d attempt(s)", attempts)
	skipped := o.dag.SkipDescendants(node.ID)
	o.fireTaskFailed(node.ID, logPrefix, skipped)
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

// fireTaskFailed fires a failure event through the state machine. Skipped
// descendants ride in the payload so the report and digest can name them
// without re-deriving the DAG.
func (o *Orchestrator) fireTaskFailed(taskID, logPrefix string, skipped []string) {
	slog.Warn(logPrefix+": task failed", "id", taskID, "skipped", skipped)
	evt := Event{
		Type:    EventTaskFailed,
		Payload: map[string]any{"task_id": taskID, "skipped": skipped},
	}
	next, err := o.state.HandleEvent(o.ctx, o, evt)
	if err != nil {
		slog.Warn("failure handler error", "task_id", taskID, "error", err)
	}
	if next != nil {
		if err := o.Transition(next); err != nil {
			slog.Warn("state transition failed", "error", err)
		}
	}
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

// failFast reports whether the run should stop at the first task failure
// instead of letting independent branches finish.
func (o *Orchestrator) failFast() bool {
	return o.cfg.Mode() == FailFast
}

// simulateCompletions marks all ready/leaf tasks as done and advances the DAG.
// Used by tests and when the daemon is unavailable (NullMnemo).
func (o *Orchestrator) simulateCompletions() {
	// First pass: mark all ready tasks as done.
	for id, node := range o.dag.nodes {
		if node.Status == status.TaskReady && len(node.ParentIDs) == 0 {
			node.Status = status.TaskDone
			o.fireTaskCompleted(id, "sim")
		}
	}

	// Second pass: now promoted tasks may be ready (was blocked, now all parents done).
	// Mark those as done too.
	for id, node := range o.dag.nodes {
		if node.Status == status.TaskReady {
			node.Status = status.TaskDone
			o.fireTaskCompleted(id, "sim")
		}
	}
}

// subscribeDaemon opens a gRPC subscription for task status changes, replacing
// polling with event-driven DAG advancement. On failure, returns an error so
// the caller can fall back to polling. The subscription runs until the DAG is
// complete or the context is cancelled.
func (o *Orchestrator) subscribeDaemon() error {
	stream, err := o.daemon.Subscribe(o.ctx, &pb.SubscribeRequest{
		AgentId: "gaap-orchestrator",
		Targets: []string{"task"},
	})
	if err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}

	slog.Info("subscribed to task events via gRPC stream")

	// Single writer: the reader goroutine owns the stream and nothing else. It
	// turns wire events into taskEvents and hands them over; every DAG mutation
	// stays on this goroutine, which is also the only one reading the DAG. The
	// previous version mutated nodes from the reader while this loop iterated
	// them, which is a data race on every status field it touched.
	events := make(chan taskEvent, 64)
	streamErr := make(chan error, 1)
	streamClosed := make(chan struct{})
	stop := make(chan struct{})
	defer close(stop)

	go func() {
		defer close(streamClosed)
		for {
			event, err := stream.Recv()
			if err != nil {
				if err != io.EOF {
					select {
					case streamErr <- fmt.Errorf("subscription stream error: %w", err):
					default:
					}
					return
				}
				slog.Info("subscription stream closed (io.EOF)")
				return
			}
			if event.Task == nil {
				continue
			}
			evt := taskEvent{
				ID:         event.Task.Id,
				Status:     status.Task(event.Task.Status),
				ResultKey:  event.Task.ResultKey,
				RetryCount: event.Task.RetryCount,
				Source:     "sub",
			}
			select {
			case events <- evt:
			case <-stop:
				return
			case <-o.ctx.Done():
				return
			}
		}
	}()

	// apply drains one event and advances the DAG. Only this goroutine calls it.
	received := 0
	apply := func() bool {
		select {
		case evt := <-events:
			received++
			o.handleTaskEvent(evt)
			o.autoCompleteSynthesis("sub:synthesis")
			return true
		default:
			return false
		}
	}

	start := time.Now()
	maxWait := time.Duration(o.cfg.MaxWaitSec) * time.Second
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()

	for {
		if o.dag.AllTasksTerminal() || (o.failFast() && o.dag.HasFailures()) {
			// Drain what the reader already produced before leaving, so a fast
			// stream is not truncated by a terminal decision made mid-flight.
			for apply() {
			}
			return nil
		}
		if time.Since(start) > maxWait {
			return fmt.Errorf("subscription timed out after %v", maxWait)
		}

		select {
		case evt := <-events:
			received++
			o.handleTaskEvent(evt)
			o.autoCompleteSynthesis("sub:synthesis")
		case <-streamClosed:
			// The reader is done: apply whatever it queued, then decide.
			for apply() {
			}
			select {
			case err := <-streamErr:
				return err
			default:
			}
			// A stream that delivered nothing means the hub is not really
			// there (NullMnemo); fail so the caller falls back to polling.
			if received == 0 {
				return fmt.Errorf("subscription stream closed without receiving any task events")
			}
			return nil
		case <-o.ctx.Done():
			return fmt.Errorf("context cancelled: %w", o.ctx.Err())
		case <-tick.C:
		}
	}
}

// Transition orchestrator to the next state.
func (o *Orchestrator) Transition(next OrchestratorState) error {
	slog.Info("orchestrator state transition",
		"from", o.state.Phase(),
		"to", next.Phase())
	o.state = next
	return next.Enter(o.ctx, o)
}
