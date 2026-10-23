# Spike 004: C3 probe — does push add value over none?

**Question (C3):** Does passive injection (push) of pipeline context improve a receiver's ability to act, versus acting on its task brief alone?

**Design:** Three arms — `none` (task brief only, today's Gaap baseline), `digest` (brief + the REAL Go `buildDigest("observer")` output, dumped by `c3_fixture_test.go` — fixture-integrity test enforces the Python harness consumes Go output verbatim), `raw` (brief + full matrix). 9 questions, qwen3.5:cloud temp 0, NOT-IN-FEED protocol. Anomalies block (circuit breakers) was implemented BEFORE the probe so the digest arm isn't strawmanned by the 002 Q10 gap.

**Run:** `go test -run TestC3FixtureIntegrity` dumps context artifacts; `python3 probe.py` runs 27 LLM calls. `C3_MODEL` selects the receiver model, `C3_OUT` the results path.

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

## Re-run 2026-10-23 (glm-5.3-flash:cloud) — the quiz is saturated

qwen3.5:397b was retired on 2026-09-25, so the original run cannot be repeated.
This run substitutes glm-5.3-flash:cloud after fixing three harness defects. It is
a **different experiment, not a repeat** of the September numbers.
Artifacts: `c3_results_glm53flash_2026-10-23.json`,
`c3_run_glm53flash_2026-10-23.log`; digest fixture `6f35afedbf08b83c`.

| arm | total | non-sanity | actionable (Q3–Q9) | ctx chars |
|---|---|---|---|---|
| none | 9/9 | 7/7 | **0/7** | 284 |
| digest | 9/9 | 7/7 | **7/7** | 1209 |
| raw | 9/9 | 7/7 | **7/7** | 2039 |

Every arm scores 9/9, and that number means nothing: the none arm's 7/7 is seven
refusals ("NOT IN FEED") matching a key that *is* the refusal. glm is stronger than
qwen on this fixture — Q6, the one question qwen's push arms failed, passes here in
both push arms — so the arms converge at the ceiling and the quiz stops
discriminating. **The quiz has no headroom left on this fixture.**

The result that survives is the one the September verdict already used: actionable
content none 0/7 vs push 7/7, with digest reaching parity with raw at 59% of its
context (1209 vs 2039 chars). The harness's new premise check confirms the control
still holds for this model: all seven state questions were refused with no feed, so
the arms are still measuring context access rather than pretraining.

### Harness defects fixed in this run

1. **Transport errors were graded as answers.** `ollama()` returned the error JSON
   as the response text, and `key in answer` then matched substrings of it. The
   retired-model error's ref UUID contains digits that satisfied Q8's key, producing
   PASSes with no model in the loop — the first attempt at this re-run scored
   `none 0/9, digest 1/9, raw 1/9` against a model that answered nothing. Failures
   now return `None`, score FAIL, and abort the run without writing results (bogus
   model → `27/27 calls failed`, exit 1, no file).
2. **Provenance is recorded, not assumed:** model and digest sha256 go into the
   results file.
3. **Empty `content` falls back to `thinking`**, because a reasoning model that
   answers in the thinking field would otherwise read as a model failure. Not
   exercised here — all 27 answers arrived in `content`.

### Known-soft question, deliberately not changed

Q6's accepted key is `["stuck", "closer"]`: it accepts both sides of a binary
question, so it can only fail by refusing, and measures nothing. Left as-is so this
run stays comparable to September's. Fixing it is a separate change.

### What this does not establish

n=1 model, n=1 run, one fixture — directional, not statistical. The C3 work-product
question is still parked.
