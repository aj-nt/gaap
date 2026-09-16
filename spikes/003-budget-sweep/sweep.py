#!/usr/bin/env python3
"""Spike 003: budget sweep — mechanical (no LLM). Where does the observer digest
start dropping substance as MAX_CHARS shrinks?"""
import sys

BASE = __file__.rsplit("/", 1)[0] + "/../001-trace-emitter"
sys.path.insert(0, BASE)
import distill as D  # noqa: E402

events = D.load_events(BASE + "/fixture.jsonl")
run, tasks = D.reduce_stream(events)

SUBSTANCE = ["SQL", "govulncheck", "13 golangci-lint"]  # the three payload facts
BLOCKERS = "blocked by"  # substring per blocked-task attribution; count instances

print(f"{'budget':>7} {'digest chars':>13} {'payload facts retained':>24} {'blocker attributions':>21}")
for budget in (300, 400, 500, 600, 700, 800, 900, 1100, 1400):
    D.MAX_CHARS = budget
    d = D.build_digest(run, tasks, "observer")
    facts = sum(1 for s in SUBSTANCE if s in d)
    blk = d.count(BLOCKERS)
    print(f"{budget:>7} {len(d):>13} {facts}/{len(SUBSTANCE):>22} {blk}/2{'':>15}")
print("\nKnee = smallest budget retaining ALL payload facts + blocker attributions.")