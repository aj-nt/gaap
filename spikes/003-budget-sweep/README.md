# Spike 003: Budget sweep

**Question:** Where is the knee — the smallest observer-digest char budget that retains all payload facts (3 substance facts) plus blocker attributions?

**Run:** `python3 sweep.py` (mechanical, no LLM).

## Results

| budget | digest chars | payload facts | blocker attributions |
|---|---|---|---|
| 300 | 327 | 1/3 | 0/2 |
| 400 | 429 | 1/3 | 1/2 |
| 500 | 511 | 1/3 | 2/2 |
| 600 | 511 | 1/3 | 2/2 |
| 700 | 747 | 2/3 | 2/2 |
| 800 | 747 | 2/3 | 2/2 |
| **900** | **861** | **3/3** | **2/2** |
| 1100 | 861 | 3/3 | 2/2 |
| 1400 | 861 | 3/3 | 2/2 |

## Verdict: VALIDATED

**Knee = 900** for this fixture class (5-task DAG, 2 with substantial findings). Below it, the priority ordering sacrifices substance in a clean progression — first the done-task findings (600→700 loses T2's lint detail... at 700 T1's SQL finding survives, T2's doesn't), then blocker attributions (500→300 loses "blocked by"). Blockers are protected earlier than findings because failed/blocked lines sort first — which is correct: an observer digest that loses attribution but keeps findings is still actionable; the reverse is not.

Note the digest saturates at 861 chars — above the knee, larger budgets buy nothing on this fixture. Budgets are per-audience config, not global truth; the knee scales with findings-per-task, not task count. A 50-task run with 3 findings would likely still knee around ~900-1000; a run where every task has findings scales linearly with findings, not tasks. That's the right scaling property — substance, not structure, is what costs characters.