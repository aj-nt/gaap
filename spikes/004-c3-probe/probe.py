#!/usr/bin/env python3
"""Spike 004: C3 probe — does push (passive injection) add value over none?

Arms (contexts built from artifacts the Go fixture test dumped, plus the task brief):
  none   — receiver task text only (today's Gaap baseline: worker gets its brief)
  digest — task text + the real Go buildDigest("observer") output (context_arm_digest.txt)
  raw    — task text + full matrix incl. transport noise (context_arm_raw.txt)

Questions are pipeline-STATE questions (not skill questions) so qwen's pretraining
cannot answer them; the 2 sanity questions are answerable from the task text in
every arm and calibrate arm equality. NOT-IN-FEED protocol, temp 0.
"""
import json
import subprocess
import os

HERE = os.path.dirname(os.path.abspath(__file__))
OLLAMA_URL = "http://localhost:11434/api/chat"
MODEL = "qwen3.5:cloud"

TASK = json.load(open(f"{HERE}/c3_scenario.json"))["receiver_task"]
DIGEST = open(f"{HERE}/context_arm_digest.txt").read()
RAW = open(f"{HERE}/context_arm_raw.txt").read()

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


def ollama(prompt):
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
        return json.loads(r.stdout)["message"]["content"].strip()
    except Exception:
        return f"<error: {r.stdout[:150]!r}>"


def main():
    rows = []
    for arm, ctx in ARMS.items():
        print(f"=== ARM [{arm}] — {len(ctx)} chars ===", flush=True)
        score = 0
        for qid, q, keys in QUIZ:
            ans = ollama(f"FEED:\n{ctx}\n\nQUESTION: {q}")
            ok = any(k in ans.lower() for k in keys[arm])
            score += ok
            rows.append({"arm": arm, "qid": qid, "ok": ok, "answer": ans[:120]})
            print(f"  {qid} {'PASS' if ok else 'FAIL'}  {ans[:100]}", flush=True)
        print(f"  --> {arm}: {score}/{len(QUIZ)}", flush=True)
    with open(f"{HERE}/c3_results.json", "w") as f:
        json.dump(rows, f, indent=2)
    print("\n=== SUMMARY ===")
    for arm in ARMS:
        s = sum(1 for r in rows if r["arm"] == arm and r["ok"])
        print(f"{arm:8} {s}/9")
    print("\nNon-sanity (Q3-Q9) — the C3 metric:")
    for arm in ARMS:
        s = sum(1 for r in rows if r["arm"] == arm and r["ok"] and r["qid"] not in ("Q1", "Q2"))
        print(f"{arm:8} {s}/7")


if __name__ == "__main__":
    main()