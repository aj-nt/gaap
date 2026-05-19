# Gaap

[![Go Report Card](https://goreportcard.com/badge/github.com/aj-nt/gaap)](https://goreportcard.com/report/github.com/aj-nt/gaap)

Model-agnostic multi-agent orchestrator on the blackboard pattern. Coordinates heterogeneous agents through shared memory (Vassago).

## Thesis

> For coordinating heterogeneous, intermittently-connected AI agents, searchable shared memory — the blackboard pattern — outperforms message-passing architectures.

Gaap doesn't dispatch tasks. It declares them. Agents discover work they're qualified for. Results land in shared memory and advance the DAG. No shared wire protocol needed. No all-agents-online requirement.

## Architecture

```
Goal → [Gaap: decompose → DAG → declare tasks] → Vassago ← [agents: discover, claim, execute, publish]
                                                                              ↑
                        [Gaap: observe results → advance DAG → synthesize] ───┘
```

## Quick Start

```bash
# Start Vassago (shared memory daemon)
vassago start

# Register an agent
vassago memory add memory agent my-agent '{"capabilities": [{"action": "static_analysis", "tools": ["golangci-lint"]}]}' 4

# Run a task
gaap run "review the codebase for security issues"
```

## License

Apache 2.0 — see [LICENSE](LICENSE).
