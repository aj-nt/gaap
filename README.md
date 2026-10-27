# Gaap

[![Go Report Card](https://goreportcard.com/badge/github.com/aj-nt/gaap)](https://goreportcard.com/report/github.com/aj-nt/gaap)

Model-agnostic multi-agent orchestrator on the blackboard pattern. Coordinates heterogeneous agents
through shared memory (Vassago).

## Governance

Gaap also hosts a governance layer for autonomous agents: the missing legal layer that sits
above an agent and enforces what it's allowed to touch. Agents self-assemble belief and social
structure, but not law, because law needs a power asymmetry peers can't produce. It has to be
imposed from outside.

| Primitive | Role |
|---|---|
| Policy engine | deny-by-default rules over tool calls |
| Kill switch | scoped, non-negotiable stop (global / agent / capability) |
| Trust score | signal on the request path |
| Append-only ledger | the record the governor reads; agents never write it |
| Enforcement layer | container (Linux) or local shim (macOS) the agent's shell can't reach around |

The agent participates in its governance and never governs itself. The allow/deny decision is
deterministic, and the LLM enters only as data, never as the judge.

The primitives live in `platform/`. See [Design: Governance-as-a-Platform](docs/design/governance-as-a-platform.md).

### The record

Every decision is appended to a hash-chained ledger, one JSON object per line, fsynced on
each append. The entry is a structural skeleton, not a transcript — it commits to the
subject identity and the call arguments with a keyed commitment (HMAC over a random
per-subject key) and stores neither. A plain hash of a name or an argument is a
re-identification oracle: hash a candidate list, confirm the match. The key removes that,
and destroying it is what an erasure actually does:

```json
{"seq":1,"time":"...","kind":"decision","subject_hash":"cfad431a...","tool":"FileWrite",
 "rule_id":"deny-etc","outcome":"deny","input_hash":"1fe0cddf...",
 "reason":"protected path","reversibility":"irreversible","algo":"sha256","hash":"ecf3e963..."}
```

Read it from a terminal:

```bash
gaap ledger verify ~/.vagent/ledger/agent.jsonl   # recompute the chain, print genesis + head
gaap ledger tail ~/.vagent/ledger/agent.jsonl     # last 20 decisions, with rule attribution
```

Chain verification catches a tampered entry or entries removed from the middle. Removing
entries from the *end* leaves a shorter, internally consistent chain — detecting that needs
an external anchor (a published head hash with independent custody), which is not built.

### Decide and erase from a terminal

The governor is not a library you call from Go. You write the rules, ask it for a decision,
and read what it recorded:

```bash
gaap govern decide --policy rules.json --subject agent-1 \
  --tool Execute --args '{"command":"rm -rf /tmp/x"}' --read-tools ReadFile
gaap ledger tail ~/.gaap/ledger.jsonl
gaap ledger verify ~/.gaap/ledger.jsonl
```

`rules.json` is the sovereign's standing orders: deny-by-default, with named rules and
optional substring predicates on arguments. JSON rather than YAML so the kernel keeps zero
parser dependencies.

Erasure is key destruction. The subject's content is deleted and the per-subject commitment
key is shredded, so the entry survives as structure while the link to the person dies — and
the chain still verifies afterwards, because the chain hash never needed the key:

```bash
gaap govern erase --subject alice@example.com --reason gdpr-request
```

`examples/demo/run.sh` runs the whole story in about ninety seconds: a read allowed, a
destructive command denied, the record inspected, the chain verified, one byte changed so the
chain fails, then an erase that leaves the record intact and unlinkable. `TestDemoFlow` runs
the same narrative in CI against the policy file the demo ships, so the script cannot claim
behaviour the code does not have.

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
- **Go 1.27+**

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

202 tests. All pass with `-race`.

## Deployment

```bash
make deploy-studio   # build + scp to studio:~/.local/bin/gaap
```

## Docs

- [Design: Multi-Agent Orchestration](docs/design/multi-agent-orchestration.md)
- [Design: Orchestrator Patterns](docs/design/orchestrator-patterns.md)
- [Design: Governance-as-a-Platform](docs/design/governance-as-a-platform.md)

## License

Apache 2.0 — see [LICENSE](LICENSE).
