package gaap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aj-nt/gaap/internal/status"
	"github.com/aj-nt/gaap/internal/worker"
	"github.com/aj-nt/vassago-sdk/client"
	pb "github.com/aj-nt/vassago-sdk/proto"
	"github.com/google/uuid"
)

// MnemoClient is the Vassago daemon client interface the orchestrator talks to.
// Re-exported from the SDK so the orchestrator's signatures stay readable.
type MnemoClient = client.MnemoClient

// DefaultModel is the model `gaap run` uses when neither --model nor
// GAAP_MODEL says otherwise.
//
// It is a named constant rather than a literal in four places because the
// previous default (glm-5.1:cloud) was retired by the provider and nothing
// noticed: decomposition fell back to the hardcoded three-task DAG, workers
// failed every task with HTTP 410, and the run reported "one or more tasks did
// not complete" as if the work had been attempted. The default has to be one
// place so the next retirement is a one-line change and a visible diff.
const DefaultModel = "deepseek-v4.1-flash:cloud"

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

	// Approval decides whether a dead-lettered task asks a human. Off by
	// default; see ApprovalPolicy.
	Approval ApprovalPolicy `yaml:"approval"`
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
	cfg        *Config
	ctx        context.Context
	daemon     MnemoClient
	decomposer *Decomposer
	synthesis  *SynthesisEngine
	dag        *DAG
	state      OrchestratorState
	goal       string
	// runID names this run: it is the task-id namespace, so a human can find a
	// run's tasks — and its approval requests — in the daemon by one string.
	runID           string
	result          *SynthesisResult
	breakerRegistry map[string]*CircuitBreaker
	workerPool      *worker.Pool
	runKey          string

	// events is the run's event queue. Every transition is driven by an event
	// read here and every DAG mutation happens on the goroutine reading it: the
	// watcher goroutines only post. That is the single-writer rule from #15,
	// applied to the driver as well as to the subscription reader.
	events chan Event
	// dispatchFeed tells the watcher which task IDs are in play. The loop writes
	// it, the watcher reads it, and neither shares a data structure with the
	// other — which is why the watcher can poll without touching the DAG.
	dispatchFeed chan string
	watchOnce    sync.Once
	stopCh       chan struct{}
	stopOnce     sync.Once
	workerStop   chan struct{}
	// pendingFailure is the dead letter the loop has not yet answered. Set by
	// failTask on the loop goroutine and consumed by waitVerdict, which is the
	// only place that decides whether a failure ends the run or asks a human.
	pendingFailure *failureRecord

	// phaseLog records every phase entered, in order. Cheap, and it is how a
	// test can assert the run followed the graph instead of merely ending.
	phaseLog []Phase
	// pollFallbacks counts subscription -> polling fallbacks. Diagnostics, and
	// the only observable a test can use to see a fallback happened.
	pollFallbacks atomic.Int32

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
		// Buffered because a single step can post several events (a promotion
		// cascade, a simulated run) before the loop reads again.
		events:       make(chan Event, 256),
		dispatchFeed: make(chan string, 512),
		stopCh:       make(chan struct{}),
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

	if o.runID == "" {
		o.runID = uuid.New().String()[:8]
	}
	slog.Info("orchestrator starting",
		"goal", goal,
		"repo", o.cfg.RepoPath,
		"run_id", o.runID,
	)

	// Post the goal and let the loop drive. No phase is inspected here: what
	// happens next is the state's answer to an event, not this function's.
	o.post(Event{Type: EventGoalReceived, Payload: map[string]any{"goal": goal}})
	return o.drive()
}

// RunID returns the name this run's tasks are namespaced under. Empty until the
// run starts.
func (o *Orchestrator) RunID() string { return o.runID }

// drive is the run's single event loop, and the only goroutine that mutates the
// DAG or the state. The watchers post; states answer events; this loop decides
// when the run is over. Adding a phase is a new state, not a new branch here.
func (o *Orchestrator) drive() error {
	defer o.stopWatcher()
	for {
		select {
		case <-o.ctx.Done():
			return o.ctx.Err()
		case evt := <-o.events:
			if err := o.step(evt); err != nil {
				return err
			}
			switch o.state.Phase() {
			case PhaseDone:
				return nil
			case PhaseFailed:
				return ErrOrchestrationFailed
			}
		}
	}
}

// step hands one event to the current state and applies the transition it asks
// for. A nil state means "stay" — the only way a phase declines to move.
func (o *Orchestrator) step(evt Event) error {
	next, err := o.state.HandleEvent(o.ctx, o, evt)
	if err != nil {
		return fmt.Errorf("%s handler: %w", o.state.Phase(), err)
	}
	if next == nil {
		return nil
	}
	return o.Transition(next)
}

// post queues an event. A full queue blocks rather than dropping one: an event
// that never arrives is a run that never finishes.
func (o *Orchestrator) post(evt Event) {
	select {
	case o.events <- evt:
	case <-o.ctx.Done():
	}
}

// stopWatcher ends the observation goroutines. Safe to call more than once.
func (o *Orchestrator) stopWatcher() {
	o.stopOnce.Do(func() {
		close(o.stopCh)
		if o.workerStop != nil {
			close(o.workerStop)
			slog.Info("auto-workers stopped")
		}
	})
}

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
	// Enter explicitly rather than through Transition: the phase is already
	// waiting, and a waiting -> waiting transition is not an edge.
	o.state = &WaitingState{}
	if err := o.state.Enter(o.ctx, o); err != nil {
		return err
	}
	return o.drive()
}

// startWatcher begins observing the daemon, once per orchestrator.
//
// Subscription when enabled, polling otherwise. A subscription that cannot open
// falls back immediately; one that closes without ever delivering an event is
// not a working subscription and also falls back, because waiting on silence is
// how a run hangs instead of failing.
func (o *Orchestrator) startWatcher() {
	o.watchOnce.Do(func() {
		o.seedWatcher()
		if o.subscribeFallbackToPoll {
			err := o.watchSubscription()
			if err == nil {
				return
			}
			slog.Info("subscription unavailable, falling back to polling", "error", err)
		}
		o.pollFallbacks.Add(1)
		go o.watchPoll()
	})
}

// seedWatcher tells the watcher which tasks are already in play. A resumed run
// re-dispatches nothing, so without this the watcher would watch an empty set and
// the run would wait out the clock.
func (o *Orchestrator) seedWatcher() {
	for _, node := range o.dag.nodes {
		if node.Status != status.TaskReady && node.Status != status.TaskClaimed {
			continue
		}
		select {
		case o.dispatchFeed <- node.ID:
		default:
			slog.Warn("dispatch feed full while seeding the watcher", "task_id", node.ID)
		}
	}
}

// watchPoll queries the daemon for the tasks this run dispatched and posts what
// it finds. It keeps its own set of IDs, fed by the loop through dispatchFeed,
// and never reads or writes the DAG.
func (o *Orchestrator) watchPoll() {
	// Detection: if the daemon is not connected at all, simulate completions.
	// The simulation mutates the DAG, so it runs on the loop, not here.
	if _, err := o.daemon.GetTask(o.ctx, "probe-detection"); err != nil && err.Error() == "vassago not connected" {
		slog.Info("polling: daemon unavailable, using simulation fallback")
		o.post(Event{Type: EventSimulate})
		return
	}

	pollInterval := time.Duration(o.cfg.PollIntervalSec) * time.Second
	maxWait := time.Duration(o.cfg.MaxWaitSec) * time.Second
	slog.Info("polling: querying daemon for task completions",
		"poll_interval_sec", o.cfg.PollIntervalSec,
		"max_wait_sec", o.cfg.MaxWaitSec)

	ids := make(map[string]struct{})
	poll := func() {
		for id := range ids {
			entry, err := o.daemon.GetTask(o.ctx, id)
			if err != nil || entry == nil {
				continue
			}
			evt := taskEventFromClient(entry)
			o.post(Event{Type: EventTaskUpdate, Task: &evt})
			if evt.Terminal() {
				delete(ids, id)
			}
		}
	}

	// Drain the IDs the loop already handed over, and look once immediately:
	// waiting a whole interval to make the first query is latency no caller asked
	// for, and it hid dispatch bugs behind a tick.
	for {
		select {
		case id := <-o.dispatchFeed:
			ids[id] = struct{}{}
			continue
		default:
		}
		break
	}
	poll()

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	start := time.Now()

	for {
		select {
		case <-o.ctx.Done():
			return
		case <-o.stopCh:
			return
		case id := <-o.dispatchFeed:
			ids[id] = struct{}{}
		case <-ticker.C:
			poll()
			if time.Since(start) > maxWait {
				slog.Warn("polling: timeout waiting for workers",
					"elapsed_sec", int(time.Since(start).Seconds()))
				o.post(Event{Type: EventTimeout})
				return
			}
		}
	}
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

// PhaseHistory returns the phases this run entered, in order.
func (o *Orchestrator) PhaseHistory() []Phase {
	return append([]Phase(nil), o.phaseLog...)
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

// waitVerdict decides where a waiting run goes after an event was applied:
// still waiting, failed, or synthesizing. One place, so the two paths that can
// advance the DAG — a watcher update and a simulated run — cannot disagree about
// when the run is over.
func (o *Orchestrator) waitVerdict() OrchestratorState {
	// A failure with a human on the other end is not the end of the run. This
	// is checked before fail_fast: fail_fast stops dispatching to other
	// branches, it does not skip the question the operator opted into.
	if st := o.approvalVerdict(); st != nil {
		return st
	}
	if o.failFast() && o.dag.HasFailures() {
		slog.Warn("fail_fast: ending run at the first failure")
		return &FailedState{}
	}
	if !o.dag.AllTasksTerminal() {
		return nil
	}
	if failed := o.dag.FailedIDs(); len(failed) > 0 {
		slog.Warn("tasks failed, transitioning to failed", "failed", failed)
		return &FailedState{}
	}
	slog.Info("all tasks complete, transitioning to synthesizing")
	return &SynthesizingState{}
}

// approvalVerdict answers the pending dead letter with a question, when the run
// opted into asking humans. nil means the failure stands as the run's answer.
//
// It consumes the failure: the request owns it now, so a second call cannot
// open a second approval for the same dead letter.
func (o *Orchestrator) approvalVerdict() OrchestratorState {
	f := o.pendingFailure
	if f == nil || !o.cfg.Approval.Enabled {
		return nil
	}
	node, ok := o.dag.nodes[f.TaskID]
	if !ok {
		return nil
	}
	o.pendingFailure = nil

	req := NewApprovalRequest(node.ID, node.AgentType, f.Attempts, node.Summary, time.Now(), o.cfg.Approval.Timeout)
	slog.Info("approval: asking a human",
		"task_id", req.TaskID, "agent_type", req.AgentType,
		"attempts", req.Attempts, "deadline", req.Deadline.Format(time.RFC3339))

	return &ApprovalState{
		Request:        req,
		ApprovalTaskID: node.ID + "_" + ApprovalAgentType,
		Deadline:       req.Deadline,
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
	// The verdict reads this to decide whether the failure ends the run or
	// becomes a question for a human. Recorded here because this is the one
	// place that sees a dead letter from either transport.
	o.pendingFailure = &failureRecord{TaskID: node.ID, Attempts: attempts}
	o.fireTaskFailed(node.ID, logPrefix, skipped)
}

// failureRecord is the failure a verdict still has to answer for.
type failureRecord struct {
	TaskID   string
	Attempts int32
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

// failFast reports whether the run should stop at the first task failure
// instead of letting independent branches finish.
func (o *Orchestrator) failFast() bool {
	return o.cfg.Mode() == FailFast
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

// watchSubscription opens a gRPC subscription for task status changes and posts
// each one. Returns an error when the stream cannot be opened, so the caller can
// fall back to polling; once open, the reader goroutine owns the stream and only
// posts — every DAG write stays on the loop.
func (o *Orchestrator) watchSubscription() error {
	stream, err := o.daemon.Subscribe(o.ctx, &pb.SubscribeRequest{
		AgentId: "gaap-orchestrator",
		Targets: []string{"task"},
	})
	if err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}

	slog.Info("subscribed to task events via gRPC stream")

	go func() {
		received := 0
		for {
			event, err := stream.Recv()
			if err != nil {
				if err != io.EOF {
					slog.Warn("subscription stream error", "error", err)
				} else {
					slog.Info("subscription stream closed (io.EOF)")
				}
				if received == 0 {
					// A stream that delivered nothing is not a working
					// subscription: fall back rather than wait on silence.
					o.pollFallbacks.Add(1)
					slog.Info("subscription delivered no events, falling back to polling")
					go o.watchPoll()
				}
				return
			}
			if event.Task == nil {
				continue
			}
			received++
			evt := taskEventFromProto(event.Task)
			o.post(Event{Type: EventTaskUpdate, Task: &evt})
		}
	}()

	// The stream can be silent for reasons that are not errors, so the wait
	// still needs a clock. This is the only timer in the subscription path.
	go func() {
		maxWait := time.Duration(o.cfg.MaxWaitSec) * time.Second
		select {
		case <-o.ctx.Done():
		case <-o.stopCh:
		case <-time.After(maxWait):
			o.post(Event{Type: EventTimeout})
		}
	}()

	return nil
}

// Transition orchestrator to the next state.
func (o *Orchestrator) Transition(next OrchestratorState) error {
	from, to := o.state.Phase(), next.Phase()
	if !allowedTransition(from, to) {
		// Refuse before moving. A transition the graph does not contain means a
		// caller is wrong, and the run should say so rather than arrive in a
		// phase it can never leave.
		return fmt.Errorf("illegal transition %s -> %s (see transitions.go)", from, to)
	}
	slog.Info("orchestrator state transition",
		"from", from,
		"to", to)
	o.phaseLog = append(o.phaseLog, to)
	o.state = next
	return next.Enter(o.ctx, o)
}
