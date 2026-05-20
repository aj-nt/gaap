# Gaap

[![Go Report Card](https://goreportcard.com/badge/github.com/aj-nt/gaap)](https://goreportcard.com/report/github.com/aj-nt/gaap)

Model-agnostic multi-agent orchestrator on the blackboard pattern. Coordinates heterogeneous agents
through shared memory (Vassago).

## Thesis

> For coordinating heterogeneous, intermittently-connected AI agents, searchable shared memory — the
> blackboard pattern — outperforms message-passing architectures.

Gaap doesn't dispatch tasks. It declares them. Agents discover work they're qualified for. Results
land in shared memory and advance the DAG. No shared wire protocol needed. No all-agents-online
requirement.

## Architecture

```
Goal → [Gaap: decompose → DAG → declare tasks] → Vassago ← [agents: discover, claim, execute, publish]
                                                                              ↑
                        [Gaap: observe results → advance DAG → synthesize] ───┘
```

The orchestrator runs as a state machine:

```
IDLE → PLANNING → WAITING → SYNTHESIZING → DONE
         ↓                          ↑
    [decompose]              [poll or subscribe]
    [build DAG]              [advance DAG]
    [dispatch tasks]         [auto-workers execute]
```

Eight design patterns govern the internals:

| Pattern | Purpose |
|---|---|
| State Pattern | Orchestrator phases |
| Template Method + Strategy | Pluggable LLM decomposition |
| Chain of Responsibility | Capability matching (exact → FTS5 → no-match) |
| Observer (Pub/Sub) | Push-based DAG advancement via gRPC |
| Composite + Strategy | Synthesis (LLM-first, schema fallback) |
| Optimistic Locking | Atomic task claims |
| Circuit Breaker | Agent failure protection |
| Command Pattern | Auditable task mutations |

## Quick Start

### Prerequisites

- **Vassago daemon** — shared memory blackboard. [github.com/aj-nt/vassago](https://github.com/aj-nt/vassago)
- **Ollama** — LLM backend for decomposition and synthesis (defaults to localhost:11434)
- **Go 1.26+**

### Build

```bash
git clone https://github.com/aj-nt/gaap.git
cd gaap
make build        # → ./gaap (macOS arm64)
make build-linux  # → ./gaap-linux-amd64
```

### Run

```bash
# Ensure Vassago daemon is running
vassago start

# Show what would happen without executing
gaap run --dry-run "audit the codebase for security issues"

# Run a real pipeline with push-based task updates
gaap run --subscribe "review the daemon/internal/grpc package for code quality"

# Run with a different model
gaap run --model claude-sonnet-4-6 --ollama-url http://192.168.86.27:11434/v1 "audit the codebase"

# Resuming after orchestrator crash (run key printed in planning logs)
gaap resume runstate_audit-the-codebase_3f7a1b2c
```

### Flags

```
gaap run [flags] <goal>

  --subscribe       Enable push-based task updates via gRPC (falls back to polling)
  --dry-run         Show decomposition without dispatching tasks
  --addr string     Vassago daemon address (default: localhost:50051)
  --repo string     Repository path to analyze (default: current directory)
  --timeout int     Max wait for workers in seconds (default: 300)
  --model string    LLM model name (default: glm-5.1:cloud)
  --ollama-url string  Ollama base URL (default: http://localhost:11434/v1)
  --max-tokens int  Max tokens for LLM responses (default: 4096)
  --temperature float  LLM temperature, 0.0-1.0 (default: 0.1)
```

## Testing

```bash
make test   # All tests with -race
make bench  # SchemaStrategy benchmarks
make cover  # Coverage profile
make ci     # fmt + vet + test (CI gate)
```

108 tests. All pass with `-race`.

## Deployment

```bash
make deploy-studio   # build + scp to studio:~/.local/bin/gaap
```

## Docs

- [Design: Multi-Agent Orchestration](docs/design/multi-agent-orchestration.md)
- [Design: Orchestrator Patterns](docs/design/orchestrator-patterns.md)

## License

Apache 2.0 — see [LICENSE](LICENSE).
