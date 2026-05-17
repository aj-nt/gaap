# Multi-Agent Orchestration on Vassago — Design Document

## Status: Draft, post-spike iteration

---

## 1. Thesis

> For coordinating heterogeneous, intermittently-connected AI agents, searchable shared memory — the blackboard pattern — outperforms message-passing architectures. The tradeoffs that killed blackboard in deterministic distributed systems (shared state races, lack of explicit causality) don't apply to stochastic, high-latency LLM agents, while its strengths (organic capability discovery, graceful degradation, built-in provenance) are exactly what agent coordination needs.

This is not "yet another agent framework." This is a **coordination substrate** — a layer that lets agents collaborate without knowing about each other, without agreeing on wire protocols, and without all being online at the same time.

---

## 2. Why Not Message Passing

Every current multi-agent system (AutoGen, CrewAI, LangGraph, Temporal) is built on message passing: Agent A sends a message to Agent B. This works when:

- All agents are online and reachable
- All agents speak the same protocol
- The DAG executor has durable storage for retries and state

It breaks when:

- Agents are transient (serverless functions, cron jobs, machines that sleep)
- Agents are heterogeneous (shell scripts, Python processes, humans, webhooks)
- You want agents to discover work they're qualified for without static routing tables
- You want to trace the provenance of a decision back through multiple agents

The blackboard pattern inverts this: instead of sending messages, agents read and write to a shared, searchable memory space. The orchestrator doesn't dispatch tasks — it declares tasks. Agents discover tasks they're qualified for. Results land in shared memory and advance the DAG.

---

## 3. Architecture

```
                         ┌──────────────────────┐
                         │   Human / Cron /      │
                         │   Webhook / CLI       │
                         └──────────┬───────────┘
                                    │ "review the vassago codebase"
                                    ▼
                         ┌──────────────────────┐
                         │     Orchestrator      │
                         │                      │
                         │  - Decomposes goals   │
                         │  - Builds task DAG    │
                         │  - Searches for       │
                         │    capable agents     │
                         │  - Writes tasks to    │
                         │    shared memory      │
                         │  - Watches for        │
                         │    results via pub/sub│
                         │  - Advances DAG       │
                         │  - Produces synthesis │
                         └──────────┬───────────┘
                                    │ gRPC
                                    ▼
                         ┌──────────────────────┐
                         │       Vassago         │
                         │                      │
                         │  - Memory (CRUD +     │
                         │    FTS5 search)       │
                         │  - Sessions           │
                         │  - Pub/sub Telepathy  │
                         │  - Agent identity     │
                         │  - Namespaces         │
                         │  - Skills registry    │
                         │  - Cron scheduling    │
                         └───┬──────┬──────┬────┘
                             │      │      │
                    ┌────────▼┐ ┌───▼───┐ ┌▼────────┐
                    │ Agent A │ │Agent B│ │ Agent C │
                    │ (shell) │ │(Python│ │ (Human) │
                    │         │ │       │ │         │
                    │ polls   │ │ polls │ │ polls   │
                    │ memory  │ │ mem   │ │ dashbrd │
                    │ for     │ │ for   │ │ for     │
                    │ tasks   │ │ tasks │ │ tasks   │
                    └─────────┘ └───────┘ └─────────┘
```

### Key design decisions:

**The orchestrator is an agent itself.** It uses an LLM for decomposition and synthesis. Its tools are Vassago memory operations, not file writes or terminal commands. It's a thin coordinator, not a heavyweight workflow engine.

**Agents communicate only through Vassago.** No direct agent-to-agent messaging in v1. This keeps the protocol surface minimal and lets agents be completely decoupled.

**Polling is the baseline.** Every agent can poll. Webhooks, gRPC push, and streaming are optimizations on top. The simplest agent is a cron job that checks for new tasks.

---

## 4. Core Primitives

### 4.1 Capability Declaration

Agents register what they can do as structured Vassago memory entries:

```json
{
  "agent_id": "spike-static",
  "capabilities": [
    {
      "action": "static_analysis",
      "tools": ["golangci-lint", "gosec", "govulncheck"],
      "languages": ["go"],
      "cost_estimate": "low",
      "typical_duration": "30s-2m"
    }
  ],
  "endpoint": null,
  "mode": "poll",
  "poll_interval": 10
}
```

The orchestrator discovers capable agents by searching Vassago:
- "Find agents that can do static_analysis in Go"
- "Find agents with gosec capability"

This is FTS5 over structured fields, not prose scanning. Each capability field maps to a searchable token.

**Spike lesson applied:** The spike used FTS5 on raw report text and hit hyphen parsing bugs. The real system indexes structured capability metadata — no column filter confusion.

### 4.2 Task Representation

Tasks are Vassago memory entries with a standard envelope:

```json
{
  "task_id": "tsk_abc123",
  "parent_ids": [],
  "status": "ready",
  "goal": "Run static analysis on vassago source tree",
  "context": {
    "source_path": "/home/aj/Documents/repos/vassago",
    "config": {
      "golangci_lint_timeout": "120s"
    }
  },
  "output_schema": {
    "type": "memory_entry",
    "target": "memory",
    "category": "project",
    "key_prefix": "task_tsk_abc123"
  },
  "assigned_agent": null,
  "created_at": 1715900000,
  "claimed_at": null,
  "completed_at": null
}
```

**Key properties:**
- `parent_ids` defines the DAG. Tasks with unresolved parents stay in `blocked` status.
- `context` is references, not payloads. Point to files, memory keys, config values. The agent fetches what it needs.
- `output_schema` tells the agent where and how to publish results. No guesswork.
- `assigned_agent` is null until an agent claims it. First-come, first-served.

**Spike lesson applied:** The spike's agents used hardcoded keys. The real system has a standard schema so agents know where to write, and the orchestrator knows where to read.

### 4.3 Claim Protocol

Agents discover and claim tasks. The claim must be atomic to prevent double-execution:

```
1. Agent polls: "search for tasks where status=ready AND capabilities matches mine"
2. Agent claims: "update task tsk_abc123 set status=claimed, assigned_agent=spike-static"
3. If claim succeeds (no conflict), agent executes
4. If claim fails (another agent got it first), agent moves to next task
```

Vassago's `writeMu` serializes writes, so the claim is naturally atomic — two agents can't claim the same task simultaneously.

The agent writes results as a new memory entry:
```
vassago memory add memory project task_tsk_abc123_result "<report content>" 4
```

Then updates the task:
```
vassago task complete tsk_abc123 --result-id <uuid>
```

The orchestrator watches for result entries via pub/sub or polling. When all parents of a blocked task have results, the orchestrator promotes it to `ready`.

### 4.4 Result Schema

To enable synthesis without an LLM for every cross-reference, results follow a standard structure:

```json
{
  "task_id": "tsk_abc123",
  "agent_id": "spike-static",
  "status": "success|partial|failed",
  "summary": "Found 3 HIGH, 12 MEDIUM, 5 LOW issues",
  "findings": [
    {
      "severity": "HIGH",
      "category": "security",
      "title": "Unchecked SQL query parameter",
      "location": "store/memory.go:142",
      "detail": "Query built with fmt.Sprintf, should use parameterized queries",
      "identifiers": ["sql-injection", "CWE-89"]
    }
  ],
  "raw_output": "<full tool output, truncated if >50KB>",
  "duration_ms": 3847,
  "claimed_at": 1715900005,
  "completed_at": 1715900009
}
```

**Spike lesson applied:** The spike's keyword matching produced zero HIGH CONFIDENCE hits because the two agents spoke completely different languages. Standardized finding categories (`security`, `performance`, `style`, `architecture`, `testing`) and shared identifiers (`CWE-89`, `sql-injection`) make cross-referencing mechanical instead of semantic.

---

## 5. The DAG Engine

The orchestrator maintains the task DAG as a state machine:

```
                    ┌──────────┐
                    │  blocked  │  (parents not all complete)
                    └─────┬────┘
                          │ all parents complete
                          ▼
                    ┌──────────┐
                    │  ready    │  (available for claiming)
                    └─────┬────┘
                          │ agent claims
                          ▼
                    ┌──────────┐
                    │  claimed  │  (agent working)
                    └─────┬────┘
                    ┌─────┴─────┐
                    │            │
                    ▼            ▼
              ┌──────────┐  ┌──────────┐
              │  done     │  │  failed   │
              └─────┬────┘  └─────┬────┘
                    │            │
                    │  ▼         │  ▼
                    │  (advance  │  (retry or
                    │   children)│   dead-letter)
                    │            │
                    └─────┬──────┘
                          │
                    results published
                    in Vassago memory
```

### Retry model (v1):
- Failed tasks retry up to 2 times with the same agent
- After 3 failures, task goes to `dead_letter`
- Dead-lettered tasks notify the human (gateway ping, dashboard alert)
- The human can reassign, edit context, or cancel

### Timeout model:
- Tasks have a configurable TTL (default: 5 minutes)
- If claimed but not completed within TTL, task resets to `ready`
- This handles crashed agents and network partitions

**Spike lesson applied:** The spike had zero failure handling. The daemon crashed and the synthesis agent timed out. The real system needs retry, TTL, and dead-letter as table stakes.

---

## 6. Orchestrator Resilience

The orchestrator is a separate process — it can crash without taking down Vassago or
agent execution. This is the blackboard advantage: state is in Vassago, not in the
orchestrator's memory.

### Crash recovery

On restart, the orchestrator reconstructs its state from Vassago:

```
1. Query all tasks with status != done
2. Rebuild DAG from parent_ids
3. For each claimed task with exceeded TTL → reset to ready
4. For each ready task → leave as-is (agents will claim)
5. For each blocked task → check if parents are now done, promote if so
6. Resume event loop
```

No replay log. No journal. The tasks table IS the state.

### Partial DAG execution

The orchestrator can crash mid-DAG and recover:

```
Scenario: Orchestrator dies after T1 and T2 complete, before T3 is promoted

Recovery:
  1. Query tasks: T1=done, T2=done, T3=blocked
  2. Rebuild DAG: T1,T2 are parents of T3
  3. Both parents done → promote T3 to ready
  4. T3 becomes claimable by agents
```

The DAG self-heals from the tasks table. No orchestrator-specific recovery logic needed.

### At-least-once semantics

Task execution is at-least-once:
- If an agent crashes after claiming but before completing, TTL resets the task
- Another agent (or the same one on restart) re-claims and re-executes
- Agents must be idempotent (or the task must tolerate re-execution)

The result schema's `task_id` field lets the orchestrator detect duplicate results
and deduplicate during synthesis.

---

## 7. Agent Protocol

### Minimum viable agent:

An agent must do exactly three things:

1. **Register capabilities** — write a memory entry describing what it can do
2. **Poll for tasks** — search for ready tasks matching its capabilities
3. **Claim, execute, publish** — claim a task, do the work, write results

That's it. No gRPC required (though the SDK makes it easier). No inbound connections needed. No shared code.

### Example: Minimum shell-script agent

```bash
#!/bin/bash
# A minimal poll-execute-publish agent

AGENT_ID="my-agent"
VASSAGO="$HOME/bin/vassago"

while true; do
    # Find a ready task
    TASK=$($VASSAGO task list --status ready --capability static_analysis --limit 1 --json)

    if [ "$TASK" = "null" ]; then
        sleep 10
        continue
    fi

    TASK_ID=$(echo "$TASK" | jq -r '.task_id')
    SRC=$(echo "$TASK" | jq -r '.context.source_path')

    # Claim it
    $VASSAGO task claim "$TASK_ID" "$AGENT_ID"

    # Do the work
    golangci-lint run "$SRC/..." > /tmp/result.txt 2>&1

    # Publish
    $VASSAGO memory add memory project "task_${TASK_ID}_result" "$(cat /tmp/result.txt)" 4

    # Mark complete
    $VASSAGO task complete "$TASK_ID"
done
```

### Optimizations on top:

- **Webhook mode**: Agent registers an HTTP endpoint. Orchestrator POSTs task when ready. No polling latency.
- **gRPC push**: Agent opens a streaming connection. Orchestrator pushes tasks via pub/sub Telepathy events.
- **Batch mode**: Agent processes N tasks per run, useful for cron-based agents that run every 5 minutes.

---

## 8. The Synthesis Problem

The spike exposed that cross-referencing findings from heterogeneous agents is the hardest part. Options:

### Option A: LLM-mediated synthesis (v1+)
The orchestrator (which is an LLM agent) reads all result entries and produces a unified report. This is the most flexible but most expensive approach. It's also the only approach that works without shared schemas.

### Option B: Schema-driven synthesis (v1)
All agents agree on finding categories and identifiers (see Section 4.4). The orchestrator cross-references mechanically: find findings with the same `category` and `identifiers` across different agent results. This works without an LLM but requires schema compliance.

### Recommendation: Start with B, add A.
Schema-driven synthesis handles the 80% case. When the orchestrator encounters findings it can't mechanically cross-reference, it escalates to LLM synthesis. This gives the best of both: cheap and reliable for well-structured output, flexible for edge cases.

---

## 9. Data Model

Resolved: separate `tasks` table (not memory entries). The spike proved the memory approach works for
proof-of-concept, but atomic claims and TTL enforcement need a dedicated schema. See Section 13,
Question 1 and Patterns Doc Section 7 (Optimistic Locking).

### tasks
| Column | Type | Description |
|--------|------|-------------|
| task_id | UUID | Primary key |
| parent_ids | JSON array | DAG dependencies; null parent_ids → leaf task |
| status | enum | blocked, ready, claimed, done, failed, dead_letter |
| goal | text | What the agent should accomplish (human-authored) |
| context | JSON | File paths, config keys, memory references — not payloads |
| output_schema | JSON | Where/how to publish results (target, category, key_prefix) |
| capability_required | text | Structured capability spec for exact matching; FTS5-indexed as fallback |
| assigned_agent | text | Agent ID that claimed it; null until claimed |
| priority | int | 1-5, higher = more urgent |
| ttl_seconds | int | Claim timeout; task resets to ready if exceeded (default: 300) |
| retry_count | int | Incremented on each failed attempt |
| owner | text | Namespace (matches existing Vassago owner column) |
| created_at | timestamp | |
| claimed_at | timestamp | |
| completed_at | timestamp | |

Indexes: unique on `task_id`, indexed on `status`, `owner`, `capability_required` (structured),
FTS5 on `goal` and `capability_required` (full-text fallback).

### agent_registry (existing memory table, structured convention)
Agents register as memory entries with standard keys. No new table needed — the existing
memory table already has `owner`, `source_agent`, `category`, and FTS5 search.
```
vassago memory add memory agent agent_id '{json capability blob}' 4 --source_agent=agent_id
```

The orchestrator discovers agents by:
1. **Exact match** — query agent entries with structured filters (action, tool, language)
2. **FTS5 fallback** — full-text search over capability content when no exact match

See Patterns Doc Section 4 (Chain of Responsibility) for the full matching pipeline.

---

## 10. What We Are NOT Building (v1)

- **No streaming progress.** Results are published as completed units. Partial progress and intermediate updates are v2.
- **No agent-to-agent messaging.** All coordination through shared memory + orchestrator. Level 3 of the multi-agent model is deferred.
- **No distributed consensus.** Single Vassago daemon. Multi-daemon replication is a separate project.
- **No dynamic capability learning.** Agents declare capabilities manually. The system doesn't infer that Agent A learned Go from watching its task history.
- **No workflow engine.** This is not Temporal. No durable execution with replay, no versioned workflows, no timers-as-a-service.
- **No agent SDK requirement.** Agents can use raw Vassago CLI calls. The SDK is optional.
- **None of the spike anti-patterns.** See Patterns Doc Section 10 for the four anti-patterns the spike accidentally implemented and the correct replacements.

---

## 11. Spike Lessons (Incorporated)

| Lesson | Design Response |
|--------|----------------|
| FTS5 hyphens break as column filters | Capability declarations use underscores, not hyphens. Structured metadata over raw FTS5 on prose. |
| UUID truncation in list output | Task operations return full UUIDs. CLI format optimized for machine readability (`--json` flag). |
| Keyword cross-reference fails with different vocabularies | Standard finding categories + identifiers in result schema. LLM synthesis as fallback. |
| Daemon crash lost no data (WAL mode) | Confirms blackboard resilience. Adds crash recovery: task TTL resets unclaimed tasks. |
| No daemon log on crash | Add structured logging to Vassago daemon. Non-negotiable. |
| Temp files as coordination hack | Tasks are first-class Vassago entities. Agents search for work; orchestrator watches for results. |
| Polling 3s intervals is coarse | TTL-based claim timeout handles the polling latency gap. Webhook/gRPC push for latency-sensitive workloads. |

---

## 12. Implementation Plan

### Phase 1: Task primitives in Vassago (1 week)
- Add `tasks` table with schema from Section 9
- Add `task` CLI commands: create, list, claim, complete, fail
- Add FTS5 index on capability_required
- Implement Optimistic Locking for atomic claims (Patterns Doc Section 7)
- Implement Command Pattern for task mutations (Patterns Doc Section 9)
- Add TTL-based claim expiry

### Phase 2: Orchestrator agent (1 week)
- Implement State Pattern skeleton (Patterns Doc Section 2)
- Build Decomposer with Template Method + Strategy (Patterns Doc Section 3)
- Build CapabilityMatcher with Chain of Responsibility (Patterns Doc Section 4)
- DAG advancement: promote children when parents complete (Section 5)
- Orchestrator resilience: crash recovery, state reconstruction (Section 6)
- Dead-letter handling: notify human on 3x failure

### Phase 3: Synthesis and demo (1 week)
- Build SynthesisEngine with Composite + Strategy (Patterns Doc Section 6)
- SchemaStrategy first for deterministic cross-referencing
- Port spike agents to use task protocol
- Build the code review demo: 3 agents, 1 DAG, shared memory
- Add a human-in-the-loop agent (dashboard claim + complete)

### Phase 4: Reliability and publish (1 week)
- Wire Observer/DAGObserver with Telepathy pub/sub (Patterns Doc Section 5)
- Add Circuit Breaker for agent failure tracking (Patterns Doc Section 8)
- Architecture writeups (these documents, finalized)
- Demo script and recording
- README / getting started guide

---

## 13. Open Questions (resolved)

1. **Tasks as memory entries or separate table?**
   **RESOLVED: Separate table.** Atomic claims, TTL enforcement, and status indexing require a dedicated `tasks` table with explicit schema. The memory entry approach (spike) proved the concept but can't support production coordination primitives without race conditions.

2. **Orchestrator as Vassago plugin or standalone process?**
   **RESOLVED: Standalone process.** The orchestrator is an agent — it writes and reads Vassago like any other agent. This keeps the daemon (Vassago) as the stable blackboard layer and lets the orchestrator use Python for LLM-heavy decomposition/synthesis. Orchestrator crashes never take down the memory layer.

3. **Pub/sub for task notifications vs. polling only?**
   **RESOLVED: Both, polling as baseline.** Every agent MUST support polling — it's the universal fallback. Agents that can hold gRPC connections (SDK users, long-lived processes) can subscribe to Telepathy events for lower latency. The orchestrator uses pub/sub. Shell-script cron agents poll.

4. **Namespace boundaries for task visibility?**
   **RESOLVED: Namespace-scoped for v1.** Tasks inherit the namespace from the creating orchestrator. Cross-namespace agent sharing requires ACL infrastructure and is deferred to v2. The existing `owner` column and gRPC metadata extraction enforce this with no new code.

---

## 14. References

- Hayes-Roth, B. (1985). "A Blackboard Architecture for Control." *Artificial Intelligence*, 26(3), 251-321.
- Nii, H.P. (1986). "Blackboard Systems: The Blackboard Model of Problem Solving and the Evolution of Blackboard Architectures." *AI Magazine*, 7(2), 38-53.
- Vassago Daemon skill: `vassago-daemon` (architecture, invariants, testing patterns)
- Multi-Agent Coordination Levels: `vassago-daemon/references/multi-agent-coordination-levels.md`
- Spike code: `~/Documents/repos/vassago/spike/` (throwaway, reference only)
