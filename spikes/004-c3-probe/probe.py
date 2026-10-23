#!/usr/bin/env python3
"""Spike 004: C3 probe — does push (passive injection) add value over none?

Arms (contexts built from artifacts the Go fixture test dumped, plus the task brief):
  none   — receiver task text only (today's Gaap baseline: worker gets its brief)
  digest — task text + the real Go buildDigest("observer") output (context_arm_digest.txt)
  raw    — task text + full matrix incl. transport noise (context_arm_raw.txt)

Scoring v2 — three outcomes, not two. v1 scored a refusal as a correct answer
whenever a refusal was what that arm should say, so the none arm reached 9/9 by
declining to say anything: "knew it" and "correctly admitted it didn't" were
worth the same point. Each question now lands in exactly one bucket:

  answered — gave content that matches. This is the C3 metric: actionable state.
  declined — refused, and refusing was right for that arm (protocol compliance).
  wrong    — refused while holding the information, or gave a non-matching answer.

Questions are pipeline-STATE questions, chosen so the receiver cannot answer them
from pretraining. Q10 and Q12 require composing facts across digest lines rather
than quoting one; Q11 is answerable only from the raw firehose, so it measures
what the 900-char budget drops instead of assuming the drop is free.

A transport error is not an answer: ollama() returns None on failure and the run
aborts without writing results. The previous version returned the error JSON as
response text, and `key in answer` scored substrings of it (the retired-model
error's ref UUID contains digits matching Q8's key), producing PASSes with no
model in the loop.

MODEL comes from $C3_MODEL and is recorded with the digest sha256, because the
original model (qwen3.5:397b) was retired 2026-09-25 — a later run is a different
experiment, not a repeat of the 2026-09-16 numbers.
"""
import hashlib
import json
import os
import subprocess
import sys
import time

HERE = os.path.dirname(os.path.abspath(__file__))
OLLAMA_URL = "http://localhost:11434/api/chat"
MODEL = os.environ.get("C3_MODEL", "glm-5.3-flash:cloud")

TASK = json.load(open(f"{HERE}/c3_scenario.json"))["receiver_task"]
DIGEST = open(f"{HERE}/context_arm_digest.txt").read()
RAW = open(f"{HERE}/context_arm_raw.txt").read()
DIGEST_SHA = hashlib.sha256(DIGEST.encode()).hexdigest()

REFUSE = "not in feed"

# accept: per-arm accepted keys, lowercase. An arm whose accepted key IS the
#         refusal was asked something it never received — declining is correct
#         there, but it scores as `declined`, never as `answered`.
# mode:   "all" requires every key present (composition), otherwise "any".
QUIZ = [
    {"id": "Q1", "q": "What vulnerability are you fixing? One key phrase.",
     "accept": {"none": ["sql", "concatenat"], "digest": ["sql", "concatenat"], "raw": ["sql", "concatenat"]}},
    {"id": "Q2", "q": "Which task found that vulnerability? Task id.",
     "accept": {"none": ["t1"], "digest": ["t1"], "raw": ["t1"]}},
    {"id": "Q3", "q": "Is the dep-audit agent type currently dispatchable, or is its circuit breaker tripped? One word: dispatchable or tripped.",
     "accept": {"none": [REFUSE], "digest": ["tripped", "open"], "raw": ["tripped", "open"]}},
    {"id": "Q4", "q": "Task T5 (cross-review) is blocked. Which task id blocks it?",
     "accept": {"none": [REFUSE], "digest": ["t3"], "raw": ["t3"]}},
    {"id": "Q5", "q": "Name one NON-SQL finding from the security scan.",
     "accept": {"none": [REFUSE], "digest": ["tls", "retry", "file paths"], "raw": ["tls", "retry", "file paths"]}},
    # Q6 key tightened to "stuck" alone: "closer or stuck" accepted both sides,
    # so the only way to fail it was to refuse — it measured silence, not judgment.
    {"id": "Q6", "q": "Is the pipeline closer to completion or to being stuck? One word: closer or stuck.",
     "accept": {"none": [REFUSE], "digest": ["stuck"], "raw": ["stuck"]}},
    {"id": "Q7", "q": "After your fix, which task verifies it? Task id.",
     "accept": {"none": [REFUSE], "digest": ["t8"], "raw": ["t8"]}},
    {"id": "Q8", "q": "How many tasks are in the pipeline?",
     "accept": {"none": [REFUSE], "digest": ["8"], "raw": ["8"]}},
    {"id": "Q9", "q": "Name any task other than T8 that is currently BLOCKED. Task id.",
     "accept": {"none": [REFUSE], "digest": ["t5", "t7"], "raw": ["t5", "t7"]}},
    # Q10 — two hops. T5 is blocked by the failed T3; T7 is blocked by T5. No
    # line says "T5 and T7 can never start" — it has to be chained.
    {"id": "Q10", "q": "If the dep-audit failure is never resolved, which tasks can never start? Name every task id.",
     "accept": {"none": [REFUSE], "digest": ["t5", "t7"], "raw": ["t5", "t7"]}, "mode": "all"},
    # Q11 — the budget cost, measured. The observer digest drops T2's lint line
    # at the 900-char boundary, so the digest arm should refuse this one.
    {"id": "Q11", "q": "Which issue did the style/lint review flag as the worst one?",
     "accept": {"none": [REFUSE], "digest": [REFUSE], "raw": ["body"]}},
    # Q12 — compare two lines: T4 exists only as a retry of T3.
    {"id": "Q12", "q": "Two tasks attempt the same check. Name both task ids.",
     "accept": {"none": [REFUSE], "digest": ["t3", "t4"], "raw": ["t3", "t4"]}, "mode": "all"},
]

ARMS = {"none": TASK, "digest": TASK + "\n\nPIPELINE DIGEST (pushed to you):\n" + DIGEST,
        "raw": TASK + "\n\nFULL PIPELINE FEED (pushed to you):\n" + RAW}

SANITY = ("Q1", "Q2")
OUTCOMES = ("answered", "declined", "wrong")


def ollama(prompt):
    """Returns (answer, source_field) on success, (None, error_text) on failure."""
    payload = json.dumps({
        "model": MODEL, "stream": False, "options": {"temperature": 0},
        "messages": [
            {"role": "system", "content": "You are a worker agent. Answer ONLY from the provided feed. If the feed does not contain the answer, say exactly: NOT IN FEED. Be terse."},
            {"role": "user", "content": prompt},
        ],
    }).encode()
    r = subprocess.run(
        ["curl", "-s", "--max-time", "150", "-X", "POST", OLLAMA_URL,
         "-H", "Content-Type: application/json", "-d", "@-"],
        input=payload, capture_output=True)
    try:
        d = json.loads(r.stdout)
    except Exception:
        return None, f"non-JSON response: {r.stdout[:150]!r}"
    if d.get("error"):
        return None, str(d["error"])[:200]
    msg = d.get("message") or {}
    content = (msg.get("content") or "").strip()
    if content:
        return content, "content"
    # Reasoning models can leave content empty and put everything in thinking;
    # grading content-only then reads as a model failure rather than a harness bug.
    thinking = (msg.get("thinking") or "").strip()
    if thinking:
        return thinking, "thinking"
    return None, "empty response (no content, no thinking)"


def classify(answer, accept, mode):
    """answered | declined | wrong — see the module docstring."""
    if answer is None:
        return "error"
    low = answer.lower()
    if REFUSE in low:
        # Correct only where the arm was asked something it never received.
        return "declined" if accept == [REFUSE] else "wrong"
    hit = all(k in low for k in accept) if mode == "all" else any(k in low for k in accept)
    return "answered" if hit else "wrong"


def main():
    out_json = os.environ.get("C3_OUT", f"{HERE}/c3_results_v2.json")
    print(f"model={MODEL}  digest_sha256={DIGEST_SHA[:16]}  questions={len(QUIZ)}", flush=True)

    rows = []
    failures = 0
    for arm, ctx in ARMS.items():
        print(f"=== ARM [{arm}] — {len(ctx)} chars ===", flush=True)
        for q in QUIZ:
            ans, meta = ollama(f"FEED:\n{ctx}\n\nQUESTION: {q['q']}")
            if ans is None:
                failures += 1
                outcome, shown = "error", f"<{meta}>"
            else:
                outcome = classify(ans, q["accept"][arm], q.get("mode", "any"))
                shown = ans[:100]
            rows.append({
                "arm": arm, "qid": q["id"], "outcome": outcome,
                "answer": None if ans is None else ans[:200],
                "field": None if ans is None else meta,
                "error": None if ans is not None else meta,
            })
            print(f"  {q['id']:4} {outcome:8} {shown}", flush=True)

    if failures:
        print(f"\nABORT: {failures}/{len(rows)} calls failed. Not writing results — "
              f"a partial run is not a measurement.", file=sys.stderr)
        return 1

    counts = {arm: {k: sum(1 for r in rows if r["arm"] == arm and r["outcome"] == k)
                    for k in OUTCOMES} for arm in ARMS}
    state_qs = [q["id"] for q in QUIZ if q["id"] not in SANITY]

    results = {
        "schema": 2,
        "model": MODEL,
        "run_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "digest_sha256": DIGEST_SHA,
        "context_chars": {arm: len(ctx) for arm, ctx in ARMS.items()},
        "counts": counts,
        "rows": rows,
    }
    with open(out_json, "w") as f:
        json.dump(results, f, indent=2)
    print(f"\nwrote {out_json}", flush=True)

    print(f"\n{'arm':8} {'answered':>9} {'declined':>9} {'wrong':>6}   of {len(QUIZ)}")
    for arm in ARMS:
        c = counts[arm]
        print(f"{arm:8} {c['answered']:>9} {c['declined']:>9} {c['wrong']:>6}")

    print(f"\nThe C3 metric — actionable state, Q3-Q12 ({len(state_qs)} questions):")
    for arm in ARMS:
        n = sum(1 for r in rows if r["arm"] == arm and r["outcome"] == "answered" and r["qid"] in state_qs)
        print(f"{arm:8} {n}/{len(state_qs)}")

    wrong = [(r["arm"], r["qid"], r["answer"][:80]) for r in rows if r["outcome"] == "wrong"]
    if wrong:
        print("\nWrong — refused with the data in hand, or non-matching:")
        for arm, qid, a in wrong:
            print(f"  {arm:7} {qid:4} {a}")

    return 0


if __name__ == "__main__":
    sys.exit(main())
