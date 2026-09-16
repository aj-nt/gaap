# Spike 004: C3 probe — does push add value over none?

**Question (C3):** Does passive injection (push) of pipeline context improve a receiver's ability to act, versus acting on its task brief alone?

**Design:** Three arms — `none` (task brief only, today's Gaap baseline), `digest` (brief + the REAL Go `buildDigest("observer")` output, dumped by `c3_fixture_test.go` — fixture-integrity test enforces the Python harness consumes Go output verbatim), `raw` (brief + full matrix). 9 questions, qwen3.5:cloud temp 0, NOT-IN-FEED protocol. Anomalies block (circuit breakers) was implemented BEFORE the probe so the digest arm isn't strawmanned by the 002 Q10 gap.

**Run:** `go test -run TestC3FixtureIntegrity` dumps context artifacts; `python3 probe.py` runs 27 LLM calls.

## Raw results (c3_run.log, c3_results.json)

| arm | total | non-sanity (Q3–Q9) | ctx chars (incl. 284-char brief) |
|---|---|---|---|
| none | 9/9 | 7/7 | 284 |
| digest | 8/9 | 6/7 | 1209 |
| raw | 8/9 | 6/7 | 2039 |

## Verdict: PARTIAL — push works and is efficient, but the quiz format structurally favors the none arm

### The grader artifact (read before the table)

The `none` arm's "perfect score" is **vacuous by construction**: its accepted key for every state question was "not in feed", so it scored full marks by *correctly declining to answer*. Declining earns protocol compliance, not capability — a receiver that knows nothing about the pipeline scores 9/9 while carrying **zero actionable state**. The honest C3 read is not "none wins", it is:

- **none: 0/7 questions carry actionable content** (all answers are refusals)
- **digest: 6/7 actionable** (Q3 breaker-tripped ✓, Q4 blocker T3 ✓, Q5 non-SQL finding ✓, Q7 verifier T8 ✓, Q8 count ✓, Q9 blocked T7 ✓)
- **raw: 6/7 actionable** — identical to digest on every question

### What was actually established

1. **Push delivers strictly more actionable state than none, at zero cost to task performance** (sanity Q1/Q2 pass in all arms). The baseline question C3 asked — does passive injection help? — is answered **yes, conditional on the task's coupling to pipeline state**: every question whose answer lived beyond the brief was answerable only in the push arms.
2. **Digest == raw on all measured questions, at 59% of raw's context** (1209 vs 2039 incl. brief; 900 vs 1717 for the pushed content). The 002 conclusion holds in the Gaap-native setting.
3. **The anomalies block closed the Q10 gap in practice**: Q3 (breaker tripped) passed in the digest arm where it failed in 002. The fixture test enforces the block's presence permanently.
4. **Q6 failed identically in both push arms** ("closer or stuck" → NOT IN FEED): the strict NOT-IN-FEED system prompt suppressed the judgment call. 002 asked the same question with the same prompt and got "stuck" — so this is protocol stochasticity on judgment questions, not a content gap. Lesson: judgment questions need a different protocol (allow judgment, require grounding), not a stricter refusal rule.

### Honest asymmetries

- The digest dropped T2's lint line (900-char budget, whole-line policy) — no question hinged on it, but the asymmetry is real and on the record.
- n=1 fixture, n=1 receiver model, n=1 run. The C3 metric is directional, not statistical.

### Recommendation

- Push (with the trace-emitter) is worth keeping ON for receivers whose work depends on pipeline state; for fully self-contained tasks it adds nothing (none arm parity on sanity questions). Default: digest push stays on; the cost is ~900 chars per advancement.
- The original Gaap confound is now fully resolved: raw context is not just unnecessary, its only unique content (anomalies) is now in the digest.
- C3's remaining unknown is the WORK-PRODUCT question (does context improve the *quality of the fix*, not quiz answers) — that needs an execution harness, not a quiz. Parked deliberately; this spike answered the state-access question it was built to answer.