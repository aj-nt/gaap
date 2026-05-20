# Changelog

All notable changes to Gaap.

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
- **Model-agnostic** — `--model`, `--ollama-url`, `--max-tokens`, `--temperature` flags; defaults to glm-5.1:cloud

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
