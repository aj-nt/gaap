package gaap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aj-nt/gaap/internal/worker"
	"github.com/aj-nt/gaap/platform"
	"github.com/aj-nt/vassago-sdk/client"
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
	// RateLimit turns a `rate_limit` rule into a deny carrying a cooldown and a
	// count. Off by default, and the rule is accepted either way: with it off the
	// call is denied with the rule's own reason, which is the pre-M8 behaviour
	// minus the load-time rejection that made the rule unwritable.
	RateLimit bool `yaml:"rate_limit"`
	// Governor decides whether a task may be dispatched at all. nil = ungoverned,
	// which is the default: a run is not governed by accident, because the
	// governor's answer changes what the run does.
	Governor *platform.Governor
	// AgentID is who the governor is deciding for. Empty means the orchestrator's
	// own id: the run is the subject of its own decisions.
	AgentID string
}

// ErrOrchestrationFailed is returned when a run finishes with at least one
// failed task. The report is still produced and persisted — the error is what
// makes the process exit non-zero instead of reporting success over a hole.
var ErrOrchestrationFailed = errors.New("orchestration failed: one or more tasks did not complete")

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
	// openQuestion is true while a human decision is pending. No verdict may end
	// the run while it is set: the events that arrive during a pause are stale
	// news about branches that failed before the question was asked, and acting
	// on them would kill a run that is waiting for an answer it still wants.
	openQuestion bool

	// pendingReviews are calls the policy would not decide on its own, oldest
	// first. Like failures they are a queue: a run that is paused on one review
	// keeps dispatching what it may, and each review is still owed an answer.
	pendingReviews []reviewRecord

	// pendingFailures are the dead letters the loop has not yet answered, oldest
	// first. A queue rather than a slot because branches fail while the run is
	// paused on a question: a slot would keep the last one and lose the rest,
	// and the watcher reports a terminal state exactly once.
	pendingFailures []failureRecord

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
	if rs.RunID != "" {
		o.runID = rs.RunID
	}
	// A resumed run keeps the approval loop it started with. The policy is not a
	// per-invocation flag any more than the failure mode is.
	if rs.ApprovalPolicy.Enabled {
		o.cfg.Approval = rs.ApprovalPolicy
	}

	if err := o.resumeFromState(rs); err != nil {
		return err
	}

	slog.Info("orchestrator resuming from saved state",
		"goal", o.goal,
		"repo", o.cfg.RepoPath,
		"task_count", o.dag.TaskCount(),
	)

	// Jump directly to the state the run was in — planning and dispatch are done.
	// Enter explicitly rather than through Transition: idle -> waiting is not an
	// edge, and the phase already is what it is.
	if rs.Pending != nil {
		o.state = &ApprovalState{
			Request:        rs.Pending.Request,
			ApprovalTaskID: rs.Pending.ApprovalTaskID,
			Deadline:       rs.Pending.Deadline,
			Resumed:        true,
		}
		// Recorded here because resume enters a state directly: a phase the run
		// is in but never transitioned into would otherwise be missing from the
		// history, which is exactly the phase a reader wants to see.
		o.phaseLog = append(o.phaseLog, o.state.Phase())
		slog.Info("orchestrator resuming into a pending approval",
			"task_id", rs.Pending.Request.TaskID,
			"approval_task", rs.Pending.ApprovalTaskID,
			"deadline", rs.Pending.Deadline.Format(time.RFC3339))
		if err := o.state.Enter(o.ctx, o); err != nil {
			return err
		}
		return o.drive()
	}
	o.state = &WaitingState{}
	o.phaseLog = append(o.phaseLog, o.state.Phase())
	if err := o.state.Enter(o.ctx, o); err != nil {
		return err
	}
	return o.drive()
}

// PhaseHistory returns the phases this run entered, in order.
func (o *Orchestrator) PhaseHistory() []Phase {
	return append([]Phase(nil), o.phaseLog...)
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

	// Save after entering, not before: a RunState is the run's shape, and the
	// shape is only final once the new phase has created whatever it owns (the
	// approval task exists, so the pending request is genuinely pending). A crash
	// between two saves resumes one phase behind, never into a phase whose work
	// was never done.
	if err := next.Enter(o.ctx, o); err != nil {
		return err
	}
	o.saveRunState(o.ctx)
	return nil
}
