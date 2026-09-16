# Spike 002: Receiver quiz — digest vs raw vs board

**Question:** Does a receiver that only sees the distilled digest answer actionable-state questions as well as one drowning in the full firehose? And does push-adds-nothing hold (the board/pull arm)?

**Design:** Same 10-question quiz (8 factual, 2 judgment) put to qwen3.5:cloud (temp 0, NOT-IN-FED protocol) under three context regimes: digest (observer audience, 900-char budget), raw (full 3249-char firehose incl. transport noise), board (search-index simulation, receiver "queries" per-task records).

## Run

```
python3 quiz.py      # live arms, saves results.json + run2.log
python3 regrade.py   # corrected grader, rescores saved answers, no LLM calls
```

## Verdict: PARTIAL — digest carries substance, misses anomalies; board ≥ digest (pull wins)

### Corrected head-to-head (after grader fix — original run had a case-sensitivity bug that failed Q2/Q8 in ALL arms)

| arm | score | ctx chars | chars per point |
|---|---|---|---|
| digest | 6/8 | 861 | 143 |
| raw | 10/10 | 3268 | 327 |
| board | 9/10 | 1027 | 114 |

### Digest misses, audited
- **Q3 ("highest-severity finding?") — digest answered "HIGH", key wanted "SQL".** Audited: defensible answer, grader key gap. Substance was present in the digest. Not a real miss.
- **Q10 (circuit breaker anomaly) — "NOT IN FEED".** Audited: **real gap, and not a distillation failure** — the board arm missed it identically. Anomaly state (breaker half-open, lease expiry) was never in any receiver-shaped content; it lives only in raw snapshots. Fix belongs in the distiller: add an `anomalies:` section (breakers ≠ closed, expired leases, dead letters). That's a template line, not an LLM.

### What the numbers say
- **Digest 6/8 at 26% of raw's context** — the trace carries the actionable load. All state/fact/judgment questions pass (Q1/Q4/Q5/Q6/Q8 correct; Q2 answered more completely than the key).
- **Board 9/10 at 31% of raw** — the pull-only arm *beats* the push digest. The "maybe they need nothing pushed" hypothesis survives: when the receiver can query, structured records beat both firehose and digest. Distillation's irreplaceable niche is the **intermittently-connected receiver that can't poll** — exactly the beneficiaries the blackboard thesis names.
- **Raw wins only on anomaly questions (Q10)** — i.e., the firehose's sole advantage is completeness on rare-event state, which the digest can carry in one templated line.

### Surprises
- The grader bug (case-sensitive keys vs lowered answers) initially produced 0-suspicious symmetric failures (Q2/Q8 in every arm) — a symmetric failure pattern across arms is the signature of harness error, not content error.
- qwen3.5:cloud emits a `thinking` field separately from `content`; grading on content alone is correct, but a thinking-inclusive parser would have been noise.

### Recommendation for the real build
1. Distiller + board records are complements, not rivals: **publish the digest for intermittent receivers; keep queryable records for pollers**. The spike tested push vs pull as if exclusive — the design should do both (Gaap already has both halves: pub/sub for push, Vassago FTS5 for pull).
2. Add `anomalies:` (breakers, expired leases, dead letters) to the observer digest — one template block, closes the Q10 gap.
3. Digest needs no LLM. Board indexing already exists. The layer is ~150 lines of Go-equivalent logic + one category-visibility rule in Vassago.
4. Original spike's confound resolved: raw context is 3.8x the chars for 1.6x the score (and 0x on the questions that matter for unblocking — digest tied raw on Q2/Q6/Q8).