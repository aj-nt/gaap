# Spike 005: Extraction probe — can the four governance primitives be lifted out of the orchestrator?

**Question (open Q1):** Does "platform replaces the orchestrator" resolve as a clean lift-and-demote of the four governance primitives (`capability.go`, `circuit_breaker.go`, `digest.go`, command pattern), or as a clean cut?

**Design:** Static read of the four claimed primitive files + `orchestrator.go` + `dag.go` + `states.go`; grep for every caller of each primitive (production and test); build + full test run to confirm the current tree is green before any conclusion.

## What is actually in each file

| Doc claimed primitive | Reality | Coupling to `*Orchestrator` |
|---|---|---|
| **Law (authorization)** = `capability.go` | A capability *matcher* (find an agent by skill), self-contained — imports only the Vassago SDK + stdlib. **Nothing calls it**; `BuildCapabilityChain` appears only in `capability_test.go`. Dead code. | Zero — extract = `mv`. |
| **Adaptive enforcement** = `circuit_breaker.go` | Genuine circuit breaker, self-contained (`sync` + `time` only). But `breakerRegistry` is **never assigned in production code** — only in three test files (`circuit_breaker_integration_test.go`, `orchestrator_digest_test.go`, `c3_fixture_test.go`). The `if o.breakerRegistry != nil` guard at `states.go:106` never fires. Dead code. | One dangling field (`Orchestrator.breakerRegistry`) + one `digest.go` reader (`breakerAnomalies`). |
| **Observability** = `digest.go` | Genuinely live, actively developed (last three commits are all trace-emitter). Methods on `*Orchestrator` reach into `o.dag`, `o.goal`, `o.breakerRegistry`. Called from `WaitingState`. | **Real coupling** — the one actual refactor. |
| **Audit** = "command pattern" | **Does not exist.** No command pattern anywhere. | — |
| *(implied)* "optimistic locking" | **Does not exist.** `DAG` is a plain unsynchronized `map` struct — no mutex, no version, no lock. | — |

## Two errors in the design doc, proven by code

1. **"Command pattern (auditable mutations)" is invented.** There is no audit trail of mutations. The "Relationship to the existing code" table overstates what exists.
2. **`capability.go` is mislabeled "law (authorization)."** A capability matcher *routes work to an agent that has a skill*; it does not *authorize an action*. Routing, not policy — calling it "law" was a category error, and it is telling that the one thing that could have been "law" was actually just matchmaking.

## Verdict: CLEAN CUT — and the premise of Q1 was off

The doc framed Q1 as "is the extraction a clean lift, or a rewrite?" The spike shows the primitives aren't load-bearing to the orchestrator at all: they are either dead (`capability.go`, `circuit_breaker.go`) or the one live-and-coupled piece is observability (`digest.go`). The orchestrator is a ~550-line decompose → dispatch → poll → synthesize state machine; the governance layer is **not** woven through it.

**The blackboard thesis never got implemented.** The capability matcher (which would let agents *discover each other by skill* on the board) was written and never wired in. The circuit breaker (which would make the board *resilient to bad agents*) was written and never populated. There is no thesis to preserve — there is a scaffold pointing at a thesis abandoned mid-build.

**"Platform replaces the orchestrator" is the only honest option.** There is nothing load-bearing to extract; there is a thin orchestrator that can either be rewritten as a governed workload on a new kernel, or shelved.

## Recommendation for the plan

1. Write the plan as a **clean-cut**, not a lift-and-demote.
2. `capability.go` and `circuit_breaker.go` are salvageable as-is by moving them (both already clean, self-contained).
3. `digest.go` is the one real refactor (decouple from the `Orchestrator` struct).
4. Strike the "command pattern + optimistic locking" rows from the doc; they do not exist.
5. The first real question the plan must answer is not "how do we extract" but **"is the orchestrator a governed workload on the new kernel, or a thing we delete?"** — it currently does nothing a simple decompose→dispatch loop wouldn't, and its two governance features were never turned on.

## Verification

- `go build ./...` → exit 0.
- `go test ./...` → `ok` across `gaap`, `cmd/gaap`, `internal/ollama`, `internal/worker`.
