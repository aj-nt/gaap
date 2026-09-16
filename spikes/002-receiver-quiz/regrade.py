#!/usr/bin/env python3
"""Spike 002 re-grade: case-insensitive rescoring of run2's saved answers (no LLM calls).
Also documents the grader bug: keys were matched case-sensitively against lowered answers."""
import json

RES = __file__.rsplit("/", 1)[0] + "/results.json"

# corrected keys (lowercase, graded against answer.lower())
KEYS = {
    "Q1": ["5"], "Q2": ["t4"], "Q3": ["sql", "injection", "concatenat"],
    "Q4": ["govulncheck"], "Q5": ["13"], "Q6": ["closer", "stuck"],
    "Q7": ["3"], "Q8": ["t3", "t4"], "Q9": ["done"], "Q10": ["dep-audit", "yes"],
}

with open(RES) as f:
    results = json.load(f)

print("CORRECTED head-to-head (case-insensitive grader):")
for r in results:
    score = sum(1 for qid, _, ans in r["detail"] if any(k in ans.lower() for k in KEYS[qid]))
    misses = [(qid, ans[:70]) for qid, _, ans in r["detail"]
              if not any(k in ans.lower() for k in KEYS[qid])]
    print(f"\n{r['arm']:8} {score}/{r['total']}  (ctx {r['context_chars']} chars)")
    for qid, ans in misses:
        print(f"   miss {qid}: {ans}")