# Changelog

All notable changes to Gaap.

## [Unreleased]

A failed task stops being reported as a finished one, the governor's control
surfaces do something when you touch them, and a run can now ask a person what
to do about a dead letter instead of giving up on the branch.

### Added
- `--policy <file>`, `--state <dir>` and `--trust-floor` on `run` and `resume` — the run is governed by the same policy file and the same durable ledger the CLI uses, so the record of what was allowed and the record of what ran are one record. Off by default: a run is not governed by accident.
- **The governor decides every dispatch.** `allow` dispatches, `review` opens an approval instead (the policy's reason is the question), `deny` fails the task with `policy deny: <reason>` and skips its descendants — before anything reaches the daemon. A deny does not ask a human: asking would make the standing orders advisory. A review with no approval loop configured is a deny, because a question nobody can answer is a hang. A task a human approved is not asked again — the approval was the decision, and a second question would make "yes" mean nothing.
- `gaap approvals <run-id>` — the questions a run is waiting on, human-readable or `--json` (one flat row per request, no envelope to hop through).
- `gaap approve <run-id> [--task <id>] [--action retry|reassign|edit|cancel] --reason <why> --agent <who>` — answer one. It refuses ambiguity (two pending requests and no `--task` is an error naming both), refuses an agent outside `--agents`, and refuses a decision with no author or no reason — every refusal lands before anything is written, because a half-answered question is worse than an unanswered one.
- `--approval`, `--approval-timeout` (900s) and `--approval-agents` on `run` — the loop is off unless asked for. An orchestrator that blocks on a person out of the box is one nobody runs unattended.
- **The approval loop itself.** A dead-lettered task used to end its branch, and with `--fail-fast` the whole run. With `--approval` the orchestrator opens a request as a daemon task under the reserved agent type `approval` (`max_retries` 0: a question nobody answered is not a failed attempt to be retried, and auto-retry would ask the same person twice), pauses the graph, and waits for a typed decision. `retry`, `reassign` and `edit` supersede the failed node — a dead-lettered task is terminal in the daemon, so "try again" has to be a new node standing where the old one stood — and `cancel` accepts the failure, which is what the run would have done without the loop. A decision that cannot be enacted fails the run loudly rather than continuing on a broken instruction.
- **A run has a name, and it is the task-id namespace.** `gaap run` logs `run_id=<8 hex>`; every task id and every approval task id is prefixed with it, so `gaap approvals <run-id>` has something to filter on. Before this the prefix was random and nobody was told it.
- `gaap govern kill --scope <agent-id|global> [--reason <code>]` — trip the kill switch, durably, and record it.
- `gaap govern status` — the state directory: tripped scopes, entry count, head.
- `gaap ledger verify --state <dir>` — verify stored content against its commitments, not just the chain.
- `gaap ledger export --out <file> <path>` — an atomic copy of a record that verifies.
- `--trust-floor` on `govern decide` — escalate allows to review below a trust score.
- `--breaker-threshold` / `--breaker-cooldown` on `run` and `resume` — per-agent-type circuit breakers (off by default).

### Changed
- **An approval deadline is absolute and its clock is its own.** The request stores a wall-clock deadline, so a restart cannot silently extend it — and a timer that outlives the approval it was set for cannot be read as the run's clock expiring, or an answered approval would kill a healthy run from the grave. An expired request is withdrawn from the daemon rather than left pending, because otherwise it keeps inviting an answer to a question nobody is listening for.
- **A superseded failure stops counting as a failure.** Once a human replaced a failed task, the run's outcome is the replacement's; counting the originals failed runs that had already recovered — which is exactly what the end-to-end test caught. The record is kept: the failed node stays `failed` and names its replacement.
- **The approval phase starts the watcher and respects `MaxWaitSec`.** The answer arrives as a task update and the run's clock lives in the watcher, and a resume enters `approving` without ever passing through `waiting` — so without this an approval whose deadline was longer than `MaxWaitSec` parked the loop with nothing left to observe.
- **A policy review is announced, not waited on.** A task held for review is never dispatched, so it produces no daemon event; without an explicit announcement the loop sat in `waiting` until its clock ran out while a question nobody had been shown went unanswered.
- **A paused run keeps watching, and no verdict ends it mid-question.** Three ways to lose a run, all of them about who decides: the approval phase dropped other branches' updates (the watcher reports a terminal state exactly once, so a dropped update is a lost update — the run resumed believing a dead branch was still running); `pendingFailure` was a single slot, so a second branch dead-lettering while the first question was open overwrote it and nothing reported it again (it is a queue, announced by the waiting phase); and three places decided the run's fate inline — `EventTaskFailed`, the terminal branch of `EventTaskCompleted`, and `applyApproval`'s cancel — each with its own copy of the rule, none aware a human was being asked. They all route through the verdict now, which refuses to end a run while a question is open.
- **A resumed run re-saves its state as it moves.** The state was written at planning only, so a crash during an approval resumed into the shape the run had before it opened — no pending request, and no way to find the question. Every transition re-saves after entering, and `LoadRunState` looks the key up in the category it was written to, because the daemon addresses memories by ID and the key printed in the log is not one.
- **A resumed run asks again only when the question is gone.** The daemon can lose the request — TTL, a delete, a restart on a fresh database — and a run that assumed otherwise waited for an answer nobody could give until its own deadline killed it. On resume the orchestrator now checks: still pending → adopt it; missing or already finished → ask again under the same id and the same deadline (a finished task produces no further update, so waiting on it is waiting on nothing).
- **`resume` carries the pending approval and the approval policy.** A run that died waiting for a human used to resume into `waiting` with no memory of the question, leaving it unanswered in the daemon with nobody told. `RunState` now records the request, its absolute deadline, the run id and the approval policy, and resume adopts the request already in the daemon instead of asking twice.
- **The worker's command filter checks simple commands, not substrings.** `commandAllowed` scanned the whole `CMD:` string for a list of substrings, and that failed in both directions. Over-broad: the one-command answer to "inventory the Go files including sizes" is `find -exec wc -l`, and the `"exec "` pattern refused it, so a real run lost turn 2 and improvised for eighteen turns with `python3` heredocs; the same list refused `grep` for a repository's kill switch (`"kill"`), `2>/dev/null` (`"> /dev/"`) and `confirm` (`"rm "`). Evadable: one worker wrote `'k'+'i'+'l'+'l'` and got its grep through in four turns. The command is now split into simple commands on unquoted `;` `&&` `||` `|` and newlines (heredoc bodies skipped: they are data) and each is checked on its program word and arguments — `find -exec <prog>` requires a read-only `<prog>`, `sed -i` is refused, `git` is read-only by subcommand, a pipe into `sh`/`bash`/`zsh` is refused. It is a blast-radius limiter, not a security boundary, and the comment now says so.
- **The worker prompt's turn budget comes from `maxTurns`.** The file-goal prompt said "a budget of 10 turns. After turn 7" and the generic one "~20 turns. After turn 15", while `maxTurns` is a constructor parameter defaulting to 20 — a run configured for 40 turns was told it had 20.
- **The default model is `deepseek-v4.1-flash:cloud`.** The previous default, `glm-5.1:cloud`, was retired by the provider on 2026-09-25, and nothing noticed: decomposition fell back to the hardcoded three-task DAG (which also had unnamespaced IDs), workers failed every task with HTTP 410, and the run ended with "one or more tasks did not complete" as though the work had been attempted. The default now lives in one exported constant, `gaap.DefaultModel`, used by `run`, `resume`, the help text and the tests.
- **`Run` is an event loop.** It posts the goal and then processes events until the run is terminal; no phase is entered because the driver checked which phase it had just left. States announce what happened (`EventTasksCreated`, `EventTasksComplete`) instead of the driver re-deriving it, and `Transition` refuses any edge the graph in `transitions.go` does not contain.

### Fixed
- **The worker read the action out of the reply instead of trusting its shape.** A model trained to emit tool calls wraps the text it is asked to emit as plain protocol, so commands arrived as `go list ./... | head -100</parameter>\n</invoke>\n</｜DSML…`. The command was correct; the turn was wasted, because the trailing `</` became a simple command and the allowlist refused it — seventeen of twenty turns in one attempt, most of the second. A second shape wasted the same budget: "Let me look at the package layout first." followed by a perfectly good `CMD:` line, nudged away because the reply did not *start* with the prefix. `parseReply` now finds the first line that begins with a protocol token, strips the leaked delimiters plus a trailing tag-name residue (`… parameter>`), and takes the command as one line — or, when the line opens a heredoc, through its terminator. `DONE`/`FAIL` bodies keep their trailing lines, because a summary is prose. Unrecognised tags are left exactly where they are: `grep '</div>' page.html` is a legitimate command and a filter is not the place to guess. When it rewrites a reply it logs `stripped tool markup`, so the leak stays visible.
- **Worker log lines did not say which task or attempt they belonged to.** `worker: running command` carried a turn number and nothing else, and a response with no `CMD:`/`DONE:`/`FAIL:` prefix logged nothing at all — the `default:` branch appended a reminder and moved on, so a run could spend a third of its turns on unparseable replies with no trace. Turn accounting across a multi-task log was guesswork. Every worker line now carries `id`, `attempt` (`RetryCount+1`) and `turn`; an unparseable response logs the turn, the budget and the first 80 characters of what the model said; exhausting the budget logs that too.
- **The worker prompt never said where commands run.** A task answered `DONE` with the finding "`/workspace` is empty, with no Go files or `go.mod`" while the executor was running commands inside the repository (`c.Dir = repoPath`) — the model had invented the path, and an empty finding set is indistinguishable downstream from "no issues found". Both prompts now carry a `WORKING DIRECTORY` block naming the resolved path and asking for relative paths; with no repo path it says the directory is unknown and to run `pwd` first.
- **A promoted task was never dispatched.** Planning dispatched roots only; when a parent completed, its children were marked `ready` in the DAG and never sent to the daemon. Every multi-level DAG therefore stalled until `--timeout`, with the daemon logging `GetTask: task not found` for a task it had never been given, and the run reporting failure for work that was never attempted. `dispatchTask` is now the single dispatch path, used for roots and for promoted children.
- **Decomposition IDs were not namespaced on every path.** The LLM-failure fallback (and the single-file short circuit) returned before the per-run prefix was applied, so a degraded run dispatched `static_analysis` / `quality_scan` / `synthesis` every time: the next run hit `UNIQUE constraint failed: tasks.id`, and when the rows already existed it silently adopted the previous run's status and results. Every path out of `Decompose` now goes through `namespaceTasks`.
- **The kill switch could not be tripped, and would not have survived the process.** `KillSwitch.Trip` had no caller outside tests, so `Governor.Decide` consulted a switch nothing could throw; the CLI also builds a governor per invocation, so an in-memory trip was forgotten before the next decision. `gaap govern kill --scope <agent-id|global>` now writes the trip to `kill.json` atomically before reporting success (one-way, corrupt file refuses to open as all-clear), records it on the ledger, and denials name the scope that fired. `gaap govern status` reports what the state directory says.
- **The trust score never came back up and was never read.** `TrustManager.Reward` had no caller and `Score()` had no reader, so the score could only ratchet down — decay plus penalties with no counterweight. An allow now rewards, and `--trust-floor` escalates an allow to review while the score sits below it (opt-in; zero disables).
- **`Committer.Match` had no caller.** The chain proved nobody edited the ledger; nothing proved the content it commits to was still the content that was decided on. `gaap ledger verify --state <dir>` now resolves every commitment back to its stored content and fails on a mismatch (erased subjects are reported as erased: key destroyed, content deleted, chain still verifying — the expected end state).
- **The circuit breaker existed and was never constructed.** `NewCircuitBreaker` had no production caller and `o.breakerRegistry` was assigned only in tests, so planning's breaker guard could not fire and the digest rendered an empty breaker section on every run. `--breaker-threshold` (or `GAAP_BREAKER_THRESHOLD`, `breaker_threshold`) now builds breakers lazily per agent type, counts dead-lettered tasks, and stops dispatch to a tripped type until its cooldown (`--breaker-cooldown`, default 60s) expires. Off by default: a breaker stops dispatch mid-run, which should be an operator decision.
- **`Ledger.Save` had no caller.** `gaap ledger export --out <file> <path>` writes an atomic, fsynced copy, refusing to export a record that does not verify.
- **The default governor recorded commitments that pointed at nothing.** `NewGovernor()` now carries a memory content store, so `Content` is populated where it used to be nil and the "refuse a decision whose content cannot be stored" rule has something behind it.
- **Dead-lettered tasks were recorded as done.** The daemon's `FailTask` ends in `dead_letter` when retries run out, and both the poll loop and the gRPC subscription wrote that into the DAG as `done`. The task's children were then promoted, synthesis ran over a result that did not exist, `AllTasksComplete` returned true, and the run exited 0 — with the task's *goal text* printed where its summary belonged. `dead_letter` now becomes `failed` at the boundary, and the mapping lives in one function because the two transports speak different SDK types and had already drifted (the push path duplicated the poll path's body).
- **Descendants of a failed task are `skipped`.** They could never run, and leaving them `blocked` meant the only way to end the run was the poll timeout. `skipped` is DAG-local: never dispatched, never written to the daemon.
- **`EventTaskFailed` now fires.** It has been declared since the state machine was written and never once emitted, which is why `FailedState`'s dead-letter counter was permanently 0 and the digest's "blocked by a failed parent" attribution could never trigger.
- **Synthesis only sees completed tasks**, and a completed task with no worker summary says `no summary reported` instead of passing its goal off as a result.
- **The subscription path no longer mutates the DAG from two goroutines.** The stream reader wrote task statuses (and, on failure, the whole descendant skip cascade) from its own goroutine while the orchestrator's loop iterated the same nodes to decide whether the run was over — a data race on every field it touched, and `eventsReceived` was read across goroutines too. The reader now only converts wire events and hands them over on a channel; all DAG mutation and every terminal decision happen on one goroutine. `TestSubscriptionIsSingleWriter` drives the reader and the loop together under `-race`.

### Changed
- **`gaap run` exits non-zero when the run fails.** A run that ends in `FailedState` returns `ErrOrchestrationFailed`. This is a breaking change for anything scripting gaap and checking only the exit status for "it ran".
- **Failure policy is configurable**: `fail_soft` (default) lets independent branches finish and reports at the end; `fail_fast` ends the run at the first failure. Select with `--fail-fast`, `GAAP_FAILURE_MODE=fail_fast`, or `failure_mode` in config. The policy is persisted in the run state and restored by `gaap resume`, so a resumed run behaves like the run it is continuing.
- **Polling ends when the DAG is terminal**, not when it is complete — a failed run now finishes in seconds instead of waiting out `--timeout` (default 300s).

## [v0.4.8] — 2026-10-27

Governance becomes something an operator can run. Until now the governor had no CLI
surface and no policy file: the only way to make it decide was to write Go.

### Governance
- **Policy files** — `gaap govern decide --policy rules.json`. JSON (not YAML) so the kernel keeps zero parser dependencies. Strict validation: unique rule IDs, typed actions, mandatory reason on any non-allow.
- **Operator verbs** — `gaap govern decide`, `gaap govern erase`, over a state directory holding the record, key ring, subject handles, and erasable content. A deny exits zero: the decision is the data, not an error.
- **Declared reads** — `--read-tools` lets the workload say which tools are pure reads; the kernel never guesses that an unknown tool is reversible.
- **Demo** — `examples/demo/run.sh`: decide, record, verify, tamper, erase, in ninety seconds against the real CLI. `TestDemoFlow` runs the same narrative in CI so the script cannot drift.

### Erasure
- **Keyed commitments** — the ledger commits to subject and arguments with HMAC over a random per-subject key instead of a bare hash, which was a re-identification oracle. Destroying the key is what an erasure does; the chain still verifies, because the chain hash never needed it.
- **Content store** — per-item storage (items scoped to a subject and keyed by the commitment pointing at them), file-backed with path-segment validation. `Governor.Content` refuses to record a decision whose content cannot be stored.
- **Durable rollback** — a failed write to the key ring or handle registry no longer leaves a tombstone or mint in memory that the disk does not hold. Without it, a retried erasure recorded success with the key still on disk.

## [v0.2.0] — 2026-05-20

Production-readiness release. Fills all identified release and production gaps.

### Security
- **TLS support** — vassago daemon now wires transportCredentials(); gaap CLI has `--tls-cert` flag
- **Bearer token auth** — new `cauth` package in vassago-sdk v0.5.0; `--api-key` flag on CLI
- **Worker heartbeat** — 15s heartbeat during task execution prevents orphan recovery from stealing claims

### Reliability
- **Integration tests against live daemon** — `orchestrator_daemon_test.go` uses bufconn for full gRPC wire-path testing (happy path + dead-letter/FailedState)
- **Config-driven agent registry** — `--agent-types` flag builds catalog dynamically; pool, decomposer, and LLM prompt all driven from same list

### Deps
- vassago-sdk upgraded to v0.5.0 (cauth, TLS, Bearer token)

## [v0.1.0] — 2026-05-20

First release. Multi-agent orchestrator with blackboard coordination, DAG planning, synthesis, and circuit breaker.

### Core -- Orchestrator

- **Goal decomposition** — natural language goals decomposed into DAGs of tasks (LLM + static fallback)
- **Blackboard coordination** — tasks declared in Vassago shared memory; agents discover, claim, execute, publish results
- **Heterogeneous agents** — no shared wire protocol needed; agents register capabilities, Gaap matches tasks to agents
- **DAG advancement** — orchestrator observes completed tasks, advances the dependency graph, synthesizes results
- **Observer/Pub-sub** — task completions pushed from daemon via gRPC subscription (`--subscribe` flag); falls back to polling on failure
- **Offline resilience** — no all-agents-online requirement; agents pick up work when connected
- **Crash recovery** — `gaap resume <run-key>` rebuilds DAG from saved RunState, continues from last checkpoint
- **Circuit breaker** — closed→open→half-open pattern per agent type; protects against dead-letter loops; auto-reset on recovery
- **Auto-workers** — in-process worker goroutines execute dispatched tasks; makes `gaap run` fully self-contained
- **Synthesis** — LLM semantic cross-referencing with schema-based fallback; outputs structured audit reports
- **Model-agnostic** — `--model`, `--ollama-url`, `--max-tokens`, `--temperature` flags; defaults to deepseek-v4.1-flash:cloud

### CLI

- **gaap run [flags] <goal>** — execute an orchestration pipeline
  - `--dry-run` — decompose without dispatching
  - `--subscribe` — enable push-based task updates via gRPC (falls back to polling)
  - `--addr` — daemon address (default: localhost:50051)
  - `--repo` — repository path to analyze
  - `--timeout` — max wait for workers in seconds (default: 300)
  - `--model`, `--ollama-url`, `--max-tokens`, `--temperature` — LLM configuration
- **gaap resume [--addr <daemon>] <run-key>** — resume a saved run after orchestrator crash
- **gaap version** — print version and exit

### Architecture (8 patterns)

| Pattern | Role |
|---|---|
| State Pattern | Orchestrator phases (idle→planning→waiting→synthesizing→done) |
| Template Method + Strategy | Goal decomposition with pluggable LLM |
| Chain of Responsibility | Capability matching: exact → FTS5 → no-match |
| Observer (Pub/Sub) | DAG advancement via gRPC subscription events |
| Composite + Strategy | Synthesis: LLM-first with schema fallback |
| Optimistic Locking | Atomic task claims via `UPDATE WHERE status='ready'` |
| Circuit Breaker | Agent failure tracking with automatic trip/reset |
| Command Pattern | Auditable task mutations |

### Development

- **123 tests** — all passing with `-race` (race detector)
- **2 CI workflows** — test+vet+build on push/PR + goreleaser on tag
- **Apache 2.0** licensed
- Depends on `vassago-sdk` v0.5.0 (public, normal `go get`)
