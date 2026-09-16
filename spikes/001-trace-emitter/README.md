# Spike 001: Trace-emitter (templated distillation)

**Question:** Given a real Gaap orchestrator event firehose (task declarations, status flips, result publications, DAG snapshots), can a pure-template distiller emit audience-scoped digests under a hard char budget, with zero raw events leaking?

**Schema fidelity:** Fixture uses Gaap's real vocabulary — `TaskSpec` (task_id/parent_ids/status/goal/agent_type from decomposer.go) and `TaskResult` (task_id/agent_type/status/summary/error from synthesis.go) — plus the event types from states.go, plus transport noise (heartbeats, lease renewals, DAG snapshots that Gaap's own state machine consumes but a receiver never needs).

## Run

```
python3 distill.py
```

## Verdict: VALIDATED (mechanism) — receiver-value pending 002

### What worked
- 3249 raw chars (20 events, incl. pure transport noise) → 1195 chars across 3 audience digests, each within budget.
- Transport noise (heartbeats, lease renewals, raw DAG snapshots) contributes **zero bytes** to any receiver digest — the noise the original spike flooded channels with is exactly the component templates drop for free.
- Blocker attribution: waiting tasks name the specific failed parent ("blocked by T3"), not just "waiting".
- Budget policy: drop-whole-lines + declared drop ("[...N lower-priority line(s) dropped, full text on board]") instead of mid-line "..." — after the mid-line version silently amputated the T2 lint findings. Declared loss > silent loss.

### What didn't
- First observer digest lost substance entirely: showed "[OK] T1 (security-scan)" with no findings. Status communicated, substance didn't. Fixed: observer audience now gets summaries like workers do.
- 600-char budget on observer is below the information floor: the T1 finding (highest-value line) gets dropped as "lowest priority". The digest's priority ordering (failures > blocked > done-with-findings) protects substance only when the budget is ≥ ~900 chars. 003 sweeps the knee.

### Surprises
- The spike's first run failed exactly the way the theory predicts: the lossy interface hid the payload. Two failure modes (status-without-substance, mid-line truncation) were both invisible until a real digest was printed.
- LSP caught a latent Pyright error (possibly-unbound `relevant`) introduced by the first budget fix — the fix after the fix was tracked-drop-counting inside the loop, not post-hoc counting.

### Recommendation for the real build
- Distiller is a pure function of (event stream, audience, budget): no LLM, no new failure mode, deterministic, testable. ~120 lines of Go-equivalent logic.
- Audience set that earned its keep: orchestrator (full matrix), worker (queue + errors), observer (findings + blockers).
- Priority order for line-dropping: failed > blocked(-by whom) > ready/claimed > done-with-findings.
- Never budget-truncate mid-line. Declare every drop.