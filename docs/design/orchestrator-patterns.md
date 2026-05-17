# Multi-Agent Orchestrator — Architectural Patterns

## Status: Draft
## Companion to: docs/design/multi-agent-orchestration.md

---

## 1. Introduction

The design document establishes the *system* architecture — thesis, primitives, data model, deployment. This document establishes the *software* architecture — the design patterns that govern how the orchestrator's internal components interact, evolve, and fail.

A pattern is not just a name. It's a deliberate choice that constrains future decisions in useful ways. Every pattern here has a documented reason, a concrete implementation sketch, and an explicit connection to the spike's lessons.

**Note on implementation language:** Code sketches are in pseudocode/Go because Go's type system makes
interfaces and patterns explicit. The orchestrator itself may be implemented in Python (per design
doc Section 3: "the orchestrator uses Python for LLM-heavy decomposition/synthesis"). The patterns
translate directly — interfaces become ABCs, goroutines become asyncio tasks, channels become queues.
Where language-specific concerns matter, they're called out explicitly.

**Note on phases:** Phase numbering matches the design doc's four-phase plan (Section 12).
The patterns document groups patterns by concern, not by phase — the phases determine build order,
not logical grouping.

---

## 2. Orchestrator State Machine: State Pattern

### Problem

The orchestrator has fundamentally different behaviors depending on what phase of work it's in: decomposing a goal, waiting for agents, synthesizing results, or handling failures. Without states, this becomes a maze of `if status == "waiting"` scattered across the codebase.

### Pattern

**State Pattern** — encapsulate each phase as a state object with its own behavior.

```
                         ┌─────────────┐
                         │   IDLE      │
                         │             │
                         │ Awaiting    │
                         │ goal input  │
                         └──────┬──────┘
                                │ receive goal
                                ▼
                         ┌─────────────┐
                         │  PLANNING   │
                         │             │
                         │ Decompose   │
                         │ goal → DAG  │
                         │ Match caps  │
                         │ Create tasks│
                         └──────┬──────┘
                                │ tasks created
                                ▼
                         ┌─────────────┐
                         │  WAITING    │
                         │             │
                         │ Monitor     │
                         │ task results│
                         │ Advance DAG │
                         └──────┬──────┘
                    ┌───────────┴───────────┐
                    │                       │
              all leaves done          timeout / dead-letter
                    │                       │
                    ▼                       ▼
            ┌─────────────┐          ┌─────────────┐
            │ SYNTHESIZING│          │   FAILED    │
            │             │          │             │
            │ Cross-ref   │          │ Notify      │
            │ findings    │          │ human       │
            │ Produce     │          │ Await       │
            │ unified     │          │ decision    │
            │ report      │          │             │
            └──────┬──────┘          └─────────────┘
                   │
                   │ synthesis complete
                   ▼
            ┌─────────────┐
            │   DONE      │
            │             │
            │ Report      │
            │ available   │
            └─────────────┘
```

### Implementation Sketch

```go
// OrchestratorState defines the interface for all states.
type OrchestratorState interface {
    // Enter is called when transitioning into this state.
    Enter(ctx context.Context, o *Orchestrator) error
    // HandleEvent processes a pub/sub event or timer tick.
    HandleEvent(ctx context.Context, o *Orchestrator, evt Event) (OrchestratorState, error)
    // Name returns a human-readable state name.
    Name() string
}

type Orchestrator struct {
    state       OrchestratorState
    goal        string
    dag         *TaskDAG
    vassago     *vassago.Client
    // ...
}

func (o *Orchestrator) Transition(next OrchestratorState) error {
    slog.Info("orchestrator state transition",
        "from", o.state.Name(),
        "to", next.Name())
    o.state = next
    return next.Enter(context.Background(), o)
}
```

Each state is a small, testable unit:

```go
type PlanningState struct{}

func (s *PlanningState) Enter(ctx context.Context, o *Orchestrator) error {
    // 1. Decompose goal into task tree
    tasks, err := o.decomposer.Decompose(ctx, o.goal)
    // 2. Match capabilities for each leaf task
    for _, t := range tasks {
        agent, err := o.capabilityMatcher.FindAgent(ctx, t.CapabilityRequired)
        t.AssignedAgent = agent
    }
    // 3. Persist tasks to Vassago
    o.dag = NewTaskDAG(tasks)
    return o.dag.Persist(ctx, o.vassago)
}

func (s *PlanningState) HandleEvent(ctx context.Context, o *Orchestrator, evt Event) (OrchestratorState, error) {
    // Planning doesn't process events — it runs synchronously and transitions
    return &WaitingState{}, nil
}
```

### Why This Pattern

- **Testable.** Each state is a standalone struct — mock the Orchestrator, exercise state transitions.
- **Extensible.** Adding a new phase (e.g., "human approval required") is a new state, not scattered if-blocks.
- **Observable.** Every transition is logged. The current state is always queryable.
- **Anti-foot-gun.** The spike's synthesis agent had a single function with polling, fetching, cross-referencing, and writing all tangled. States force separation.

---

## 3. Goal Decomposition: Template Method + Strategy

### Problem

Decomposition is the orchestrator's most LLM-dependent operation. But the *pipeline* around it — validate input, call LLM, parse output, validate DAG — is fixed regardless of which LLM or prompting strategy we use.

### Pattern

**Template Method** — the decomposition pipeline is invariant. The LLM call is the variable step, injected as a Strategy.

```
┌──────────────────────────────────────────────────────────┐
│                   Decompose(goal)                         │
│                                                          │
│  1. ValidateInput(goal)        ← invariant               │
│         │                                                │
│         ▼                                                │
│  2. BuildPrompt(goal)          ← invariant               │
│         │                                                │
│         ▼                                                │
│  3. CallLLM(prompt)            ← STRATEGY (pluggable)    │
│         │                                                │
│         ▼                                                │
│  4. ParseDAG(llmOutput)        ← invariant               │
│         │                                                │
│         ▼                                                │
│  5. ValidateDAG(tasks)         ← invariant               │
│         │                                                │
│         ▼                                                │
│  6. Return(tasks)              ← invariant               │
└──────────────────────────────────────────────────────────┘
```

### Implementation Sketch

```go
// DecomposerStrategy defines how to call an LLM for decomposition.
type DecomposerStrategy interface {
    Decompose(ctx context.Context, prompt string) (string, error)
}

// Decomposer orchestrates the decomposition pipeline.
type Decomposer struct {
    strategy DecomposerStrategy
}

func (d *Decomposer) Decompose(ctx context.Context, goal string) ([]Task, error) {
    // Step 1: Validate
    if err := validateGoal(goal); err != nil {
        return nil, fmt.Errorf("invalid goal: %w", err)
    }

    // Step 2: Build prompt
    prompt := buildDecompositionPrompt(goal)

    // Step 3: Call LLM (Strategy)
    rawOutput, err := d.strategy.Decompose(ctx, prompt)
    if err != nil {
        return nil, fmt.Errorf("LLM call failed: %w", err)
    }

    // Step 4: Parse DAG from LLM output
    tasks, err := parseDAGFromJSON(rawOutput)
    if err != nil {
        return nil, fmt.Errorf("parse DAG output: %w", err)
    }

    // Step 5: Validate
    if err := validateDAG(tasks); err != nil {
        return nil, fmt.Errorf("invalid DAG: %w", err)
    }

    return tasks, nil
}
```

Strategies can be swapped:

```go
// Direct LLM call to a provider.
type DirectLLMStrategy struct {
    client *openai.Client
    model  string
}

// Local file-based decomposition (testing, offline).
type StaticDecomposition struct {
    mapping map[string][]Task  // goal → pre-baked DAG
}
```

### Why This Pattern

- **LLM-agnostic.** Swap providers, models, or prompting without touching the pipeline.
- **Testable.** Test the pipeline with a StaticDecomposition Strategy — no LLM needed.
- **Observable.** Log at each pipeline step. When decomposition fails, you know which step.
- **Spike connection.** The spike's decomposition was implicit in the synthesis script. A proper Template Method would have caught the empty DAG edge case before it reached polling.

---

## 4. Capability Matching: Chain of Responsibility

### Problem

Finding the right agent for a task is a search problem that degrades gracefully. The ideal case is an exact capability match. The fallback is a keyword search. The last resort is asking a human or guessing. These are nested fallbacks, not parallel options.

### Pattern

**Chain of Responsibility** — each handler tries to match. If it can't, it passes to the next.

```
┌──────────────────────┐
│  ExactMatchHandler   │  "static_analysis + go + gosec"
│  (structured query)  │  → exact capability field match
└──────────┬───────────┘
           │ no match
           ▼
┌──────────────────────┐
│  FTS5FallbackHandler │  "static_analysis OR golangci-lint OR gosec"
│  (keyword search)    │  → FTS5 across capability content
└──────────┬───────────┘
           │ no match
           ▼
┌──────────────────────┐
│  LLMGuessHandler     │  "What agent could handle static analysis in Go?"
│  (semantic match)    │  → LLM suggests agent from registry
└──────────┬───────────┘
           │ no match
           ▼
┌──────────────────────┐
│  HumanEscalateHandler│  "No agent found for: static_analysis in Go"
│  (ask user)          │  → Notify human, await input
└──────────────────────┘
```

### Implementation Sketch

```go
type CapabilityMatcher interface {
    FindAgent(ctx context.Context, required string) (string, error)
    SetNext(CapabilityMatcher)
}

type BaseMatcher struct {
    next CapabilityMatcher
}

func (b *BaseMatcher) SetNext(m CapabilityMatcher) {
    b.next = m
}

func (b *BaseMatcher) tryNext(ctx context.Context, required string) (string, error) {
    if b.next == nil {
        return "", fmt.Errorf("no agent found for: %s", required)
    }
    return b.next.FindAgent(ctx, required)
}

// ExactMatchHandler queries the agent registry with structured filters.
type ExactMatchHandler struct {
    BaseMatcher
    vassago *vassago.Client
}

func (h *ExactMatchHandler) FindAgent(ctx context.Context, required string) (string, error) {
    // Parse required into fields: action, tool, language
    action, tool, lang := parseCapability(required)

    // Query Vassago agent registry with structured filters
    agents, err := h.vassago.FindAgents(ctx, vassago.AgentFilter{
        Action:   action,
        Tool:     tool,
        Language: lang,
    })
    if err != nil || len(agents) == 0 {
        slog.Debug("exact match failed, falling back", "required", required)
        return h.tryNext(ctx, required)
    }
    slog.Info("exact capability match", "agent", agents[0].ID, "required", required)
    return agents[0].ID, nil
}

// FTS5FallbackHandler searches agent memory entries by keyword.
type FTS5FallbackHandler struct {
    BaseMatcher
    vassago *vassago.Client
}

func (h *FTS5FallbackHandler) FindAgent(ctx context.Context, required string) (string, error) {
    // Build FTS5 query from required tokens (OR-separated, never hyphenated)
    query := buildFTS5Query(required)
    agents, err := h.vassago.SearchAgents(ctx, query)
    if err != nil || len(agents) == 0 {
        return h.tryNext(ctx, required)
    }
    slog.Info("FTS5 capability match", "agent", agents[0].ID, "query", query)
    return agents[0].ID, nil
}
```

### Why This Pattern

- **Graceful degradation.** The most reliable handler runs first. Fallbacks are progressively more expensive or uncertain.
- **Observable.** Each handler logs its attempt. You can see which handler resolved the match.
- **Configurable.** In testing, inject a chain that stops at ExactMatch. In production, use all four.
- **Spike connection.** The spike's `find_memory_by_key` was a single FTS5 call that broke on hyphens. A chain would have caught the error and fallen back to `list` (a fourth handler we didn't need because we patched the bug instead).

---

## 5. DAG Advancement: Observer Pattern (Pub/Sub)

### Problem

The DAG is a dependency graph. When a leaf task completes, its children may become ready. The orchestrator needs to detect this without polling every task's status on a timer.

### Pattern

**Observer** — the orchestrator subscribes to Vassago's Telepathy pub/sub system. When a task completes, the daemon publishes an event. The orchestrator's observer checks the DAG and promotes any newly-unblocked children.

```
┌──────────────┐     ┌──────────────┐     ┌──────────────┐
│  Agent A     │     │  Agent B     │     │  Agent C     │
│  completes   │     │  completes   │     │  waiting on  │
│  task T1     │     │  task T2     │     │  T1 AND T2   │
└──────┬───────┘     └──────┬───────┘     └──────▲───────┘
       │                    │                    │
       ▼                    ▼                    │
┌──────────────────────────────────────┐         │
│           Vassago Daemon              │         │
│                                      │         │
│  T1 completed → pub/sub event        │         │
│  T2 completed → pub/sub event        │         │
└──────────────┬───────────────────────┘         │
               │                                 │
               │ Telepathy events                │
               ▼                                 │
┌──────────────────────────────┐                 │
│       Orchestrator            │                 │
│                              │                 │
│  onTaskCompleted(T1):        │                 │
│    check DAG → T3 still      │                 │
│    blocked (T2 pending)      │                 │
│                              │                 │
│  onTaskCompleted(T2):        │                 │
│    check DAG → T3 parents    │─────────────────┘
│    all done → promote T3     │  promote to ready
│    to ready                  │
└──────────────────────────────┘
```

### Implementation Sketch

```go
// DAGObserver subscribes to task completion events and advances the DAG.
type DAGObserver struct {
    vassago *vassago.Client
    dag     *TaskDAG
}

func (o *DAGObserver) Start(ctx context.Context) error {
    events, err := o.vassago.Subscribe(ctx, vassago.SubscriptionFilter{
        EventTypes: []string{"task.completed"},
    })
    if err != nil {
        return fmt.Errorf("subscribe: %w", err)
    }

    for {
        select {
        case <-ctx.Done():
            return ctx.Err()
        case evt := <-events:
            o.handleCompletion(ctx, evt)
        }
    }
}

func (o *DAGObserver) handleCompletion(ctx context.Context, evt Event) {
    taskID := evt.Payload["task_id"].(string)
    slog.Info("task completed, checking DAG", "task_id", taskID)

    // Find all children of this task
    children := o.dag.ChildrenOf(taskID)
    for _, child := range children {
        if o.dag.AllParentsComplete(child) {
            slog.Info("promoting task to ready", "task_id", child.ID)
            o.dag.PromoteToReady(ctx, child)
        }
    }
}
```

### Why This Pattern

- **Event-driven, not polling.** The orchestrator reacts to completions, doesn't scan.
- **Natural with Vassago Telepathy.** The daemon already has pub/sub. The orchestrator is just another subscriber.
- **Extensible.** Add observers for task failures (retry/alert), timeouts (reset), new agents (re-match).
- **Spike connection.** The spike's orchestrator polled every 3 seconds. An observer would have reacted instantly when both agents finished. The polling loop was wasted cycles and added latency.

---

## 6. Synthesis: Composite + Strategy

### Problem

The spike's hardest lesson: cross-referencing findings from heterogeneous agents requires both structured matching AND semantic understanding. Neither alone is sufficient.

### Pattern

**Composite** — findings are organized as a tree. Leaf findings come from individual agents. Composite findings are synthesized from multiple leaves.

**Strategy** — two synthesis strategies, selected by finding type: SchemaStrategy for well-structured output, LLMStrategy for everything else.

```
                    ┌──────────────────────┐
                    │   Synthesis Engine    │
                    └──────────┬───────────┘
                               │
                    ┌──────────┴───────────┐
                    │                      │
                    ▼                      ▼
          ┌─────────────────┐    ┌─────────────────┐
          │ SchemaStrategy  │    │  LLMStrategy    │
          │                 │    │                 │
          │ Match by:       │    │ Match by:       │
          │ - category      │    │ - semantic      │
          │ - identifiers   │    │   similarity    │
          │ - severity      │    │ - contextual    │
          │                 │    │   overlap       │
          │ Cheap, fast,    │    │                 │
          │ deterministic   │    │ Expensive, slow,│
          │                 │    │ non-deterministic│
          └────────┬────────┘    └────────┬────────┘
                   │                      │
                   └──────────┬───────────┘
                              │
                              ▼
                    ┌──────────────────────┐
                    │   SynthesisResult     │
                    │                      │
                    │  ├─ HighConfidence    │
                    │  │   ├─ Finding A     │
                    │  │   │   (Schema matched)│
                    │  │   └─ Finding B     │
                    │  │       (LLM matched)│
                    │  ├─ StaticOnly         │
                    │  └─ ReviewOnly         │
                    └──────────────────────┘
```

### Implementation Sketch

```go
// FindingNode is the Composite base — both leaves and composites.
type FindingNode interface {
    Severity() string
    Summary() string
    Children() []FindingNode
}

// AgentFinding is a leaf — a single finding from one agent.
type AgentFinding struct {
    AgentID     string
    Category    string
    Identifiers []string
    Detail      string
}

// SynthesizedFinding is a composite — multiple agent findings merged.
type SynthesizedFinding struct {
    Confidence string         // "HIGH", "MEDIUM"
    Sources    []AgentFinding // the leaves that contributed
    Summary    string
}

// SynthesisStrategy determines how to group findings.
type SynthesisStrategy interface {
    CanHandle(a, b *AgentFinding) bool
    Synthesize(findings []AgentFinding) *SynthesizedFinding
}

// SchemaStrategy matches by shared categories and identifiers.
type SchemaStrategy struct{}

func (s *SchemaStrategy) CanHandle(a, b *AgentFinding) bool {
    // Match if they share at least one category AND one identifier
    commonCat := intersectStringSlices(
        []string{a.Category}, []string{b.Category})
    commonID := intersectStringSlices(a.Identifiers, b.Identifiers)
    return len(commonCat) > 0 && len(commonID) > 0
}

// LLMStrategy uses an LLM to determine semantic overlap.
// Only runs on findings that SchemaStrategy couldn't match —
// never on findings SchemaStrategy already handled.
type LLMStrategy struct {
    client *openai.Client
}

func (s *LLMStrategy) CanHandle(a, b *AgentFinding) bool {
    // LLMStrategy is the fallback — use only when SchemaStrategy failed.
    // The SynthesisEngine ensures this by only passing unmatched findings.
    return true
}

func (s *LLMStrategy) Synthesize(findings []AgentFinding) *SynthesizedFinding {
    prompt := buildSynthesisPrompt(findings)
    response, _ := s.client.Complete(prompt)
    return parseSynthesis(response)
}

// SynthesisEngine runs both strategies and merges results.
type SynthesisEngine struct {
    schema  *SchemaStrategy
    llm     *LLMStrategy
}

func (e *SynthesisEngine) Synthesize(allFindings []AgentFinding) *SynthesisResult {
    result := &SynthesisResult{}

    // First pass: SchemaStrategy (cheap, fast)
    schemaGroups := e.groupBySchema(allFindings)
    for _, group := range schemaGroups {
        result.HighConfidence = append(result.HighConfidence,
            e.schema.Synthesize(group))
    }

    // Second pass: LLMStrategy on remaining unmatched findings
    unmatched := e.filterUnmatched(allFindings, schemaGroups)
    if len(unmatched) >= 2 {
        llmGroups := e.groupByLLM(unmatched)
        for _, group := range llmGroups {
            result.HighConfidence = append(result.HighConfidence,
                e.llm.Synthesize(group))
        }
    }

    // Remaining singletons are agent-only findings
    result.StaticOnly, result.ReviewOnly = e.partitionSingletons(unmatched)
    return result
}
```

### Why This Pattern

- **Cost-aware.** Schema matching is free. LLM matching is paid. Run cheap first, expensive second.
- **Composable.** The Composite tree renders naturally to a markdown report — depth-first traversal.
- **Progressive enhancement.** Start with SchemaStrategy only. Add LLMStrategy when you need it.
- **Spike connection.** The spike had zero SchemaStrategy — everything was keyword matching, which produced zero HIGH CONFIDENCE hits. A schema with `category: security` and `identifiers: [CWE-89]` would have matched instantly.

---

## 7. Task Claim: Optimistic Locking

### Problem

Two agents polling for tasks simultaneously may find the same ready task and both try to claim it. Only one should succeed.

### Pattern

**Optimistic Locking** — claim by atomic update with a condition. The loser detects the conflict and retries with a different task.

```
Agent A: UPDATE tasks SET status='claimed', assigned_agent='A'
         WHERE task_id='T1' AND status='ready'
         → 1 row updated (success!)

Agent B: UPDATE tasks SET status='claimed', assigned_agent='B'
         WHERE task_id='T1' AND status='ready'
         → 0 rows updated (T1 is already 'claimed')

Agent B detects 0 rows → searches for next ready task
```

### Implementation Sketch

```go
func (s *Store) ClaimTask(ctx context.Context, taskID, agentID string) (bool, error) {
    s.writeMu.Lock()
    defer s.writeMu.Unlock()

    result, err := s.db.ExecContext(ctx, `
        UPDATE tasks
        SET status = 'claimed',
            assigned_agent = ?,
            claimed_at = ?
        WHERE task_id = ?
          AND status = 'ready'
    `, agentID, time.Now().Unix(), taskID)

    if err != nil {
        return false, fmt.Errorf("claim task: %w", err)
    }

    rows, _ := result.RowsAffected()
    return rows == 1, nil
}

// Agent-side claim loop
func (a *Agent) claimAndExecute(ctx context.Context) error {
    tasks, _ := a.findReadyTasks(ctx)
    for _, t := range tasks {
        claimed, err := a.vassago.ClaimTask(ctx, t.ID, a.ID)
        if err != nil || !claimed {
            // Another agent beat us — try next task
            continue
        }
        // We own it — execute
        result := a.execute(ctx, t)
        a.publishResult(ctx, t.ID, result)
        return nil
    }
    return fmt.Errorf("no claimable tasks")
}
```

### Why This Pattern

- **No distributed lock manager.** The SQLite writeMu is local but the WHERE clause is the real guard.
- **Self-correcting.** A failed claim is just a signal to try the next task. No error state.
- **Testable.** Inject two agents, point them at the same task, verify exactly one claims it.
- **Spike connection.** The spike didn't need claims — tasks were hardcoded. But the design doc's claim protocol (Section 4.3) depends on exactly this atomicity guarantee.

---

## 8. Failure Handling: Circuit Breaker + Retry Queue

### Problem

Agents fail. Some failures are transient (timeout, network blip) and should retry. Some are permanent (agent crashed, broken credential) and should escalate. The orchestrator must distinguish them without knowing internal agent state.

### Pattern

**Circuit Breaker** — track consecutive failures per agent. After N failures, stop dispatching to that agent and alert.

**Retry with Exponential Backoff** — transient failures retry with increasing delays.

```
                    ┌──────────────┐
                    │   READY      │
                    └──────┬───────┘
                           │ agent claims
                           ▼
                    ┌──────────────┐
                    │   CLAIMED    │
                    └──────┬───────┘
                           │
              ┌────────────┴────────────┐
              │                         │
              ▼                         ▼
       ┌──────────────┐          ┌──────────────┐
       │   DONE       │          │   FAILED     │
       └──────────────┘          └──────┬───────┘
                                        │
                              ┌─────────┴─────────┐
                              │                   │
                        retry_count < 2      retry_count >= 2
                              │                   │
                              ▼                   ▼
                       ┌──────────────┐    ┌──────────────┐
                       │   READY      │    │  DEAD_LETTER │
                       │ (backoff:    │    │              │
                       │  2^retry s)  │    │ Notify human │
                       └──────────────┘    └──────────────┘
```

### Implementation Sketch

```go
type CircuitBreaker struct {
    mu              sync.Mutex
    failures        map[string]int       // agentID → consecutive failures
    state           map[string]string    // agentID → "closed" | "open" | "half-open"
    maxFailures     int                  // trip threshold
    resetAfter      time.Duration        // auto-reset after
}

func (cb *CircuitBreaker) RecordFailure(agentID string) {
    cb.mu.Lock()
    defer cb.mu.Unlock()

    cb.failures[agentID]++
    if cb.failures[agentID] >= cb.maxFailures {
        cb.state[agentID] = "open"
        slog.Warn("circuit breaker opened for agent",
            "agent", agentID,
            "failures", cb.failures[agentID])
        // Notify human
        cb.notifyHuman(agentID)
    }
}

func (cb *CircuitBreaker) AllowDispatch(agentID string) bool {
    cb.mu.Lock()
    defer cb.mu.Unlock()

    state := cb.state[agentID]
    switch state {
    case "closed", "":
        return true
    case "open":
        return false
    case "half-open":
        return true // let one through to test
    }
    return false
}

// RetryWithBackoff computes the delay before retrying a failed task.
func RetryWithBackoff(retryCount int) time.Duration {
    if retryCount == 0 {
        return 0 // immediate first retry
    }
    // 1s, 2s, 4s, 8s, max 60s
    d := time.Duration(1<<uint(retryCount)) * time.Second
    if d > 60*time.Second {
        d = 60 * time.Second
    }
    return d
}
```

### Why This Pattern

- **Prevents cascading failures.** A broken agent doesn't consume tasks and fail silently forever.
- **Self-healing.** Half-open state lets the circuit test if the agent recovered.
- **Human-readable.** Circuit open = dashboard alert. Human decides: fix agent, reassign tasks, or kill the agent.
- **Spike connection.** The spike's daemon crashed and we didn't know until the synthesis timed out. A circuit breaker on the orchestrator would have detected the daemon failure and alerted immediately, not after 60 seconds of polling.

---

## 9. Command Pattern: Task Operations

### Problem

Every task mutation (create, claim, complete, fail, promote) is a distinct operation that needs to be logged, potentially retried, and reversible for debugging.

### Pattern

**Command** — encapsulate each operation as a command object with an `Execute()` method. Commands are serializable (for audit log) and composable (batch operations).

### Implementation Sketch

```go
// TaskCommand is the Command interface.
type TaskCommand interface {
    Execute(ctx context.Context, store *Store) error
    Description() string // for logging
}

// CreateTaskCommand creates a new task in Vassago.
type CreateTaskCommand struct {
    TaskID             string
    Goal               string
    ParentIDs          []string
    CapabilityRequired string
}

func (c *CreateTaskCommand) Execute(ctx context.Context, store *Store) error {
    slog.Info("executing command", "command", "CreateTask", "task_id", c.TaskID)
    return store.CreateTask(ctx, store.CreateTaskParams{
        ID:                 c.TaskID,
        Goal:               c.Goal,
        ParentIDs:          c.ParentIDs,
        CapabilityRequired: c.CapabilityRequired,
        Status:             c.initialStatus(),
    })
}

func (c *CreateTaskCommand) Description() string {
    return fmt.Sprintf("CreateTask(%s): %s", c.TaskID, truncate(c.Goal, 60))
}

// ClaimTaskCommand atomically claims a task.
type ClaimTaskCommand struct {
    TaskID  string
    AgentID string
}

func (c *ClaimTaskCommand) Execute(ctx context.Context, store *Store) error {
    ok, err := store.ClaimTask(ctx, c.TaskID, c.AgentID)
    if err != nil {
        return err
    }
    if !ok {
        return fmt.Errorf("task %s already claimed", c.TaskID)
    }
    return nil
}

// BatchCommand executes multiple commands in sequence.
type BatchCommand struct {
    Commands []TaskCommand
}

func (b *BatchCommand) Execute(ctx context.Context, store *Store) error {
    for _, cmd := range b.Commands {
        if err := cmd.Execute(ctx, store); err != nil {
            slog.Error("batch command failed",
                "command", cmd.Description(),
                "error", err)
            return fmt.Errorf("batch aborted at %s: %w", cmd.Description(), err)
        }
    }
    return nil
}
```

The orchestrator's PlanningState uses a BatchCommand:

```go
func (s *PlanningState) Enter(ctx context.Context, o *Orchestrator) error {
    tasks, _ := o.decomposer.Decompose(ctx, o.goal)

    batch := &BatchCommand{}
    for _, t := range tasks {
        batch.Commands = append(batch.Commands, &CreateTaskCommand{
            TaskID:             t.ID,
            Goal:               t.Goal,
            ParentIDs:          t.ParentIDs,
            CapabilityRequired: t.CapabilityRequired,
        })
    }
    return batch.Execute(ctx, o.store)
}
```

### Why This Pattern

- **Audit trail.** Every command is logged with its description. The log IS the audit trail.
- **Undo support (v2).** Commands can implement `Undo()` for rollback. Not in v1, but the pattern makes it natural.
- **Testable.** Test each command in isolation. Mock the store. Verify Execute().
- **Spike connection.** The spike's agents called `vassago memory add` directly — no command logging, no auditable operations. If an agent wrote garbage, we had no record of what changed.

---

## 10. Anti-Patterns (Explicitly Avoided)

These are patterns the spike accidentally implemented. They're documented here so we recognize and avoid them.

### Anti-Pattern 1: Temp Files as Coordination Signals

**What the spike did:** Agents wrote UUIDs to `/tmp/spike-static-uuid.txt`. The orchestrator polled for them.

**Why it's wrong:** Temp files are invisible to the daemon. If the daemon restarts, the files are stale. If two orchestrators run, they race on the same file. The coordination surface is filesystem, not Vassago.

**Correct pattern:** Tasks are first-class Vassago entities. The orchestrator searches for task completions via store queries or pub/sub events.

### Anti-Pattern 2: FTS5 on Uncontrolled Prose

**What the spike did:** Searched for `spike-static-report` and got `no such column: static` because FTS5 parsed the hyphen as a column filter.

**Why it's wrong:** FTS5 has a query syntax. User-generated content with hyphens, colons, and special characters will randomly break. FTS5 is for indexed text fields, not arbitrary key-value discovery.

**Correct pattern:** Structured fields for machine-readable identifiers (task IDs, agent IDs, capability names). FTS5 for human-authored prose (goal descriptions, finding details). Never FTS5 on a hyphenated key expecting it to work as an exact match.

### Anti-Pattern 3: Single-Function Orchestrator

**What the spike did:** `spike-synthesis.py` was 170 lines in `main()`. Everything — polling, fetching, cross-referencing, report generation, persistence — was one function.

**Why it's wrong:** Untestable in isolation. Can't swap the cross-reference strategy without touching persistence code. Can't test polling logic without a real daemon.

**Correct pattern:** State Pattern separates concerns. Template Method separates pipeline from strategy. Each component is independently testable.

### Anti-Pattern 4: CLI Display Output as Machine Interface

**What the spike did:** Parsed `vassago memory list` output with regex to extract 8-char truncated UUIDs, then tried to pass those to `vassago memory get`.

**Why it's wrong:** CLI display output is for humans. UUIDs are truncated for readability. Field order changes with version bumps. Regex parsing is fragile.

**Correct pattern:** The `--json` flag on every CLI command. The gRPC API for programmatic access. Never parse human-readable output.

---

## 11. Pattern Interaction Map

How these patterns connect at runtime:

```
Human/Trigger
      │
      ▼
┌─────────────────────────────────────────────────┐
│              Orchestrator (State Pattern)         │
│                                                   │
│  IDLE → PLANNING                                  │
│    │                                               │
│    │ Decomposer (Template Method + Strategy)       │
│    │   → LLM decomposes goal into Task[]           │
│    │                                               │
│    │ CapabilityMatcher (Chain of Responsibility)   │
│    │   → finds agent for each task                 │
│    │                                               │
│    │ BatchCommand (Command Pattern)                │
│    │   → persists tasks to Vassago                 │
│    │                                               │
│    ▼                                               │
│  WAITING                                           │
│    │                                               │
│    │ DAGObserver (Observer)                        │
│    │   → subscribes to task.completed events       │
│    │   → promotes children when parents done       │
│    │                                               │
│    │ CircuitBreaker                                │
│    │   → watches agent failures                    │
│    │   → trips on consecutive errors               │
│    │                                               │
│    ▼                                               │
│  SYNTHESIZING                                      │
│    │                                               │
│    │ SynthesisEngine (Composite + Strategy)        │
│    │   → SchemaStrategy for structured matches     │
│    │   → LLMStrategy for semantic overlap          │
│    │   → produces SynthesisResult tree             │
│    │                                               │
│    ▼                                               │
│  DONE                                              │
└───────────────────────────────────────────────────┘
         │
         │ Optimistic Locking (used by every task mutation)
         │ gRPC calls to Vassago for all state interactions
         ▼
   ┌──────────┐
   │ Vassago  │
   │ Daemon   │
   └──────────┘
```

---

## 12. Testing Strategy

Each pattern enables a specific testing approach. Together they make the orchestrator
testable at every level without requiring a live Vassago daemon or LLM.

### Unit testing by pattern

| Pattern | How to test | Mock surface |
|---------|-------------|--------------|
| State Pattern | Instantiate state, call Enter(), assert orchestator fields changed | Orchestrator struct fields |
| Template Method | Inject StaticDecomposition strategy, verify pipeline steps in order | DecomposerStrategy |
| Chain of Responsibility | Build a 2-link chain (ExactMatch + FTS5), verify fallback fires on miss | CapabilityMatcher |
| Observer | Publish event to test channel, assert DAGObserver promoted correct task | Event channel |
| Composite | Build a tree of AgentFindings + SynthesizedFindings, call Summary(), traverse Children() | None needed (pure data) |
| SynthesisStrategy | Pass findings with matching identifiers, assert CanHandle() true for SchemaStrategy | None needed |
| Optimistic Locking | In-memory SQLite with two goroutines trying to claim same task simultaneously | Store (SQLite) |
| Circuit Breaker | RecordFailure N times, assert AllowDispatch returns false, reset, assert half-open | None needed (in-memory state) |
| Command Pattern | Create command, call Execute on mock store, assert correct params passed to store method | Store |

### Integration testing

The `StaticDecomposition` strategy and in-memory SQLite let us run the full orchestrator
pipeline without external dependencies:

```go
func TestOrchestratorEndToEnd(t *testing.T) {
    // Setup
    store := newTestStore(t)  // in-memory SQLite
    decomposer := &Decomposer{
        strategy: &StaticDecomposition{
            mapping: map[string][]Task{
                "review vassago": {
                    {ID: "T1", Goal: "static analysis", ParentIDs: nil},
                    {ID: "T2", Goal: "code review", ParentIDs: nil},
                    {ID: "T3", Goal: "synthesis", ParentIDs: []string{"T1", "T2"}},
                },
            },
        },
    }
    orch := NewOrchestrator(store, decomposer)

    // Run planning
    err := orch.Transition(&PlanningState{})
    assert.NoError(t, err)
    assert.Equal(t, "WAITING", orch.state.Name())

    // Two tasks should be ready, one blocked
    ready := store.ListTasksByStatus("ready")
    assert.Len(t, ready, 2)
    blocked := store.ListTasksByStatus("blocked")
    assert.Len(t, blocked, 1)

    // Simulate agent A claiming and completing T1
    orch.ClaimTask("T1", "agent-a")
    orch.CompleteTask("T1", "agent-a", `{"findings": [...]}`)

    // T3 should still be blocked (T2 pending)
    assert.Equal(t, "blocked", store.GetTask("T3").Status)

    // Simulate agent B completing T2
    orch.ClaimTask("T2", "agent-b")
    orch.CompleteTask("T2", "agent-b", `{"findings": [...]}`)

    // T3 should now be ready
    assert.Equal(t, "ready", store.GetTask("T3").Status)
}
```

### Property-based tests

For the DAG engine specifically, property-based testing catches edge cases:
- "Any DAG where all leaves complete eventually reaches DONE"
- "No DAG where any node depends on itself (cycle detection)"
- "For any task, ChildrenOf(ParentsOf(t)) includes t and its siblings"

---

## 13. Concurrency Model

The orchestrator has three concurrent concerns that must not interfere.

### Goroutine layout (if Go) / asyncio task layout (if Python)

```
Main event loop
├── Subscriber goroutine: Telepathy pub/sub → event channel
├── Timer goroutine: TTL expiry check every 30s
└── State execution goroutine: processes events one at a time
```

**Serial state execution.** Only ONE goroutine executes state transitions at a time.
Events are queued on a channel and processed sequentially. This eliminates race
conditions on the DAG and orchestrator state — no locks needed inside state handlers.

**Pub/sub is the only external input.** No polling. No HTTP endpoints. The event
channel is the single source of truth for what's happening in the system.

```go
func (o *Orchestrator) Run(ctx context.Context) error {
    events := make(chan Event, 100)  // buffered for bursts

    // Pub/sub subscriber
    go o.subscribeToTelepathy(ctx, events)

    // TTL checker
    go o.checkTTLs(ctx, events)

    // Serial event processing
    for {
        select {
        case <-ctx.Done():
            return ctx.Err()
        case evt := <-events:
            next, err := o.state.HandleEvent(ctx, o, evt)
            if err != nil {
                slog.Error("state handler error", "state", o.state.Name(), "error", err)
                continue
            }
            if next != nil {
                o.Transition(next)
            }
        }
    }
}
```

### Python equivalent (asyncio)

```python
async def run(self):
    events = asyncio.Queue(maxsize=100)

    # Pub/sub subscriber
    asyncio.create_task(self._subscribe_telepathy(events))

    # TTL checker
    asyncio.create_task(self._check_ttls(events))

    # Serial event processing
    while True:
        evt = await events.get()
        try:
            next_state = await self.state.handle_event(self, evt)
            if next_state:
                await self.transition(next_state)
        except Exception as e:
            logger.error("state handler error", state=self.state.name, error=e)
```

### Thread safety guarantees

- **No shared mutable state between goroutines.** The event channel is the only communication path.
- **State transitions are atomic.** A transition completes fully before the next event is processed.
- **Vassago gRPC calls are blocking.** They happen inside state handlers, which run serially, so
  they don't need to be thread-safe. The daemon handles concurrent access via writeMu.

### Deadlock prevention

The only potential deadlock is a state handler waiting for an event that can only arrive
after the handler completes. Prevention: state handlers NEVER block waiting for events.
They subscribe, publish, and return. The event loop delivers the response on the next
iteration.

---

## 14. Implementation Priorities

Note: these match the design doc's four-phase plan (Section 12). Each phase pulls in the
patterns needed at that stage.

**Phase 1 (tasks table):** Optimistic Locking, Command Pattern
The tasks table needs atomic claim and status transitions from day one.

**Phase 2 (orchestrator core):** State Pattern, Template Method, Chain of Responsibility
These are the orchestrator's skeleton. Without them, it's a script, not a system.
Note: Observer is deferred to Phase 4 — Phase 2 uses polling for DAG advancement.

**Phase 3 (synthesis and demo):** Composite + Strategy
Start with SchemaStrategy only. Add LLMStrategy when the demo needs it.

**Phase 4 (reliability):** Observer, Circuit Breaker
Pub/sub for DAG advancement. Circuit breaker once there are real agents to fail.
