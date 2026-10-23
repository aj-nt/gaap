#!/usr/bin/env python3
"""Spike 004: C3 probe — does push (passive injection) add value over none?

Arms (contexts built from artifacts the Go fixture test dumped, plus the task brief):
  none   — receiver task text only (today's Gaap baseline: worker gets its brief)
  digest — task text + the real Go buildDigest("observer") output (context_arm_digest.txt)
  raw    — task text + full matrix incl. transport noise (context_arm_raw.txt)

Questions are pipeline-STATE questions (not skill questions), chosen so the
receiver model cannot answer them from pretraining; the 2 sanity questions are
answerable from the task text in every arm and calibrate arm equality.
NOT-IN-FEED protocol, temp 0.

MODEL comes from $C3_MODEL and is recorded in the results along with the digest's
sha256, because the original model (qwen3.5:397b) was retired 2026-09-25: a later
run is a different experiment, not a repeat of the 2026-09-16 numbers.

A transport error is not an answer. ollama() returns None on failure and the row
scores FAIL — the previous version returned the error JSON as text, and the
`key in answer` matcher then scored substrings of it (the error's ref hash
contains a digit matching Q8's key), producing PASSes with no model in the loop.
The run aborts without writing results if any call failed.
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

# (id, question, {arm: [accepted keys, lowercase]}) — none-arm keys may be
# "not in feed" (correct discipline) where the question is unanswerable.
QUIZ = [
    ("Q1", "What vulnerability are you fixing? One key phrase.", {
        "none": ["sql", "concatenat"], "digest": ["sql", "concatenat"], "raw": ["sql", "concatenat"]}),
    ("Q2", "Which task found that vulnerability? Task id.", {
        "none": ["t1"], "digest": ["t1"], "raw": ["t1"]}),
    ("Q3", "Is the dep-audit agent type currently dispatchable, or is its circuit breaker tripped? One word: dispatchable or tripped.", {
        "none": ["not in feed"], "digest": ["tripped", "open"], "raw": ["tripped", "open"]}),
    ("Q4", "Task T5 (cross-review) is blocked. Which task id blocks it?", {
        "none": ["not in feed"], "digest": ["t3"], "raw": ["t3"]}),
    ("Q5", "Name one NON-SQL finding from the security scan.", {
        "none": ["not in feed"], "digest": ["tls", "retry", "file paths"], "raw": ["tls", "retry", "file paths"]}),
    ("Q6", "Is the pipeline closer to completion or to being stuck? One word: closer or stuck.", {
        "none": ["not in feed"], "digest": ["stuck", "closer"], "raw": ["stuck", "closer"]}),
    ("Q7", "After your fix, which task verifies it? Task id.", {
        "none": ["not in feed"], "digest": ["t8"], "raw": ["t8"]}),
    ("Q8", "How many tasks are in the pipeline?", {
        "none": ["not in feed"], "digest": ["8"], "raw": ["8"]}),
    ("Q9", "Name any task other than T8 that is currently BLOCKED. Task id.", {
        "none": ["not in feed"], "digest": ["t5", "t7"], "raw": ["t5", "t7"]}),
]

ARMS = {"none": TASK, "digest": TASK + "\n\nPIPELINE DIGEST (pushed to you):\n" + DIGEST,
        "raw": TASK + "\n\nFULL PIPELINE FEED (pushed to you):\n" + RAW}

SANITY = ("Q1", "Q2")


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
    # Reasoning models can leave content empty and put everything in thinking.
    # Grading content-only then reads as a model failure rather than a harness bug.
    thinking = (msg.get("thinking") or "").strip()
    if thinking:
        return thinking, "thinking"
    return None, "empty response (no content, no thinking)"


def main():
    out_json = os.environ.get("C3_OUT", f"{HERE}/c3_results.json")
    print(f"model={MODEL}  digest_sha256={DIGEST_SHA[:16]}  arms={list(ARMS)}", flush=True)

    rows = []
    failures = 0
    for arm, ctx in ARMS.items():
        print(f"=== ARM [{arm}] — {len(ctx)} chars ===", flush=True)
        score = 0
        for qid, q, keys in QUIZ:
            ans, meta = ollama(f"FEED:\n{ctx}\n\nQUESTION: {q}")
            if ans is None:
                failures += 1
                ok = False
                shown = f"<{meta}>"
                src = "error"
            else:
                ok = any(k in ans.lower() for k in keys[arm])
                shown = ans[:100]
                src = meta
            score += ok
            rows.append({
                "arm": arm, "qid": qid, "ok": bool(ok),
                "answer": None if ans is None else ans[:160],
                "field": None if ans is None else meta,
                "error": None if ans is not None else meta,
            })
            print(f"  {qid} {'PASS' if ok else 'FAIL'}  [{src}] {shown}", flush=True)
        print(f"  --> {arm}: {score}/{len(QUIZ)}", flush=True)

    if failures:
        print(f"\nABORT: {failures}/{len(rows)} calls failed. Not writing results — "
              f"a partial run is not a measurement.", file=sys.stderr)
        return 1

    # Premise check: the quiz is valid only if a receiver with NO feed cannot
    # answer the state questions. Any none-arm answer that is not a refusal means
    # the question was answerable from pretraining, and the arms stopped
    # measuring context access.
    violations = [(r["qid"], r["answer"][:100]) for r in rows
                  if r["arm"] == "none" and r["qid"] not in SANITY
                  and r["answer"] and "not in feed" not in r["answer"].lower()]

    results = {
        "model": MODEL,
        "run_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "digest_sha256": DIGEST_SHA,
        "context_chars": {arm: len(ctx) for arm, ctx in ARMS.items()},
        "premise_violations": [{"qid": q, "answer": a} for q, a in violations],
        "rows": rows,
    }
    with open(out_json, "w") as f:
        json.dump(results, f, indent=2)
    print(f"\nwrote {out_json}", flush=True)

    print("\n=== SUMMARY ===")
    for arm in ARMS:
        s = sum(1 for r in rows if r["arm"] == arm and r["ok"])
        print(f"{arm:8} {s}/9")
    print("\nNon-sanity (Q3-Q9) — the C3 metric:")
    for arm in ARMS:
        s = sum(1 for r in rows if r["arm"] == arm and r["ok"] and r["qid"] not in SANITY)
        print(f"{arm:8} {s}/7")

    if violations:
        print(f"\n!! PREMISE BROKEN: {len(violations)}/7 state questions answered "
              f"without a feed. The none arm is not blind to this model, so the arm "
              f"comparison does not measure context access:")
        for q, a in violations:
            print(f"   {q}: {a}")
    else:
        print("\nPremise holds: every state question was refused in the none arm.")

    return 0


if __name__ == "__main__":
    sys.exit(main())
