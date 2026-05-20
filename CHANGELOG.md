# Changelog

All notable changes to Gaap.

## [Unreleased]

Gaap is a model-agnostic multi-agent orchestrator on the blackboard pattern. Not yet released.

### Core -- Orchestrator

- **Goal decomposition** -- natural language goals decomposed into DAGs of tasks
- **Blackboard coordination** -- tasks declared in Vassago shared memory; agents discover, claim, execute, publish results
- **Heterogeneous agents** -- no shared wire protocol needed. Agents register capabilities; Gaap matches tasks to agents
- **DAG advancement** -- orchestrator observes completed tasks, advances the dependency graph, synthesizes results
- **Offline resilience** -- no all-agents-online requirement. Agents pick up work when connected

### Development

- **97 tests** -- all passing with `-race` (race detector)
- **2 CI workflows** -- test+vet+build on push/PR + goreleaser on tag
- **Apache 2.0** licensed
