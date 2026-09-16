#!/usr/bin/env python3
"""Spike 002: the hammer for the distillation layer.

Does a receiver that ONLY sees the distilled digest answer actionable-state
questions as well as one that sees the full firehose? Three arms:
  digest  — observer-audience digest only (the trace)
  raw     — full firehose (the matrix)
  board   — no push; receiver must query a search index (pull-only world)

Quiz: 10 questions. 8 factual state, 2 judgment (the actionable ones).
LLM: local Ollama, default model qwen3.5:cloud, temperature 0. Throwaway.
"""
import json
import subprocess
import sys

FIXTURE = __file__.rsplit("/", 1)[0] + "/../001-trace-emitter/fixture.jsonl"
DISTILL = __file__.rsplit("/", 1)[0] + "/../001-trace-emitter/distill.py"
OLLAMA_URL = "http://localhost:11434/api/chat"
MODEL = "qwen3.5:cloud"

QUIZ = [
    # (id, question, grader: substring answer(s) that earn the point — case-insensitive)
    ("Q1", "How many tasks are in the pipeline?", ["5"]),
    ("Q2", "Which task is currently blocked on another task that FAILED? Answer with the task id.", ["T4"]),
    ("Q3", "What is the highest-severity security finding T1 published? One key word.", ["sql", "injection", "concatenat"]),
    ("Q4", "What specific tool or command caused task T3 to fail?", ["govulncheck"]),
    ("Q5", "How many lint issues did the style review find?", ["13"]),
    ("Q6", "Is the pipeline closer to completion or to being stuck? One word: closer or stuck.", ["closer", "stuck"]),  # judgment
    ("Q7", "How many distinct workers appear in the feed?", ["3"]),
    ("Q8", "Which task should a fresh worker pick up next to unblock the most downstream work? Task id.", ["T3", "T4"]),  # judgment
    ("Q9", "What was the status of task T2 the last time it changed?", ["done"]),
    ("Q10", "Is any circuit breaker in an abnormal state? Which agent type?", ["dep-audit", "yes"]),
]

QUIZ_DIGEST = [q for q in QUIZ if q[0] not in ("Q7", "Q9")]  # not in digest


def ollama(prompt):
    payload = json.dumps({
        "model": MODEL, "stream": False, "options": {"temperature": 0},
        "messages": [
            {"role": "system", "content": "Answer ONLY from the provided feed. If the feed does not contain the answer, say exactly: NOT IN FEED. Be terse."},
            {"role": "user", "content": prompt},
        ],
    }).encode()
    r = subprocess.run(
        ["curl", "-s", "--max-time", "120", "-X", "POST", OLLAMA_URL,
         "-H", "Content-Type: application/json", "-d", "@-"],
        input=payload, capture_output=True)
    try:
        return json.loads(r.stdout)["message"]["content"].strip()
    except Exception:
        return f"<curl/parse error: {r.stdout[:200]!r}>"


def grade(answer, keys):
    a = answer.lower()
    return any(k in a for k in keys)


def run_arm(name, context_builder, quiz):
    ctx = context_builder()
    print(f"\n=== ARM [{name}] — context {len(ctx)} chars ===")
    score = 0
    detail = []
    for qid, q, keys in quiz:
        ans = ollama(f"FEED:\n{ctx}\n\nQUESTION: {q}")
        ok = grade(ans, keys)
        score += ok
        detail.append((qid, ok, ans[:90]))
        print(f"  {qid} {'PASS' if ok else 'FAIL'}  {ans[:90]}")
    print(f"  --> {name}: {score}/{len(quiz)}")
    return {"arm": name, "score": score, "total": len(quiz), "context_chars": len(ctx), "detail": detail}


def main():
    sys.path.insert(0, DISTILL.rsplit("/", 1)[0])
    import distill as D

    events = D.load_events(FIXTURE)
    run, tasks = D.reduce_stream(events)

    # regenerate the observer digest at 900 chars so the arm isn't strawmanned by budget
    D.MAX_CHARS = 900
    digest = D.build_digest(run, tasks, "observer")
    raw = "\n".join(json.dumps(e) for e in events)

    # board arm: same digestable facts, but as an index the receiver must query —
    # simulate pull-only by giving the LLM a search-tool-like Q&A over per-task records
    board_docs = [f"task {tid}: goal={t['goal']}; status={t['status']}; summary={t['summary'] or 'none'}; error={t['error'] or 'none'}"
                  for tid, t in tasks.items()]
    board_docs.append(f"run goal: {run['goal']}")
    board_docs.append("workers seen: w-01, w-02, w-03")

    def arm_digest():
        return digest

    def arm_raw():
        return raw

    def arm_board():
        return "SEARCH INDEX (query it by asking; full records below are what a targeted query would return):\n" + "\n".join(board_docs)

    results = []
    results.append(run_arm("digest", arm_digest, QUIZ_DIGEST))
    results.append(run_arm("raw", arm_raw, QUIZ))
    results.append(run_arm("board", arm_board, QUIZ))

    print("\n=== HEAD-TO-HEAD ===")
    print(f"{'arm':10} {'score':8} {'ctx chars':10}")
    for r in results:
        print(f"{r['arm']:10} {r['score']}/{r['total']}     {r['context_chars']}")
    with open(__file__.rsplit("/", 1)[0] + "/results.json", "w") as f:
        json.dump(results, f, indent=2)


if __name__ == "__main__":
    main()