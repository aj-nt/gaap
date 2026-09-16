#!/usr/bin/env python3
"""Spike 001: templated trace-emitter — distill a Gaap event stream into audience-scoped digests.

Reads fixture.jsonl (orchestrator event firehose), emits per-audience digests.
No LLM, no config, hardcoded everything — throwaway by design.
"""
import json
import sys
from collections import defaultdict

FIXTURE = __file__.replace("distill.py", "fixture.jsonl")

MAX_CHARS = 600          # per-audience digest budget
MAX_SUMMARY = 160        # per-result summary truncation
MAX_TASKS_IN_DIGEST = 12 # cap listed tasks per digest


def load_events(path):
    events = []
    with open(path) as f:
        for line in f:
            line = line.strip()
            if line:
                events.append(json.loads(line))
    return events


def reduce_stream(events):
    """Fold the firehose into per-task + per-run state. This is the 'trace' operation:
    the small window onto the big matrix."""
    run = {"goal": "", "run_id": ""}
    tasks = {}  # task_id -> {"goal","agent_type","status","summary","error"}
    for e in events:
        t = e["type"]
        if t == "run_meta":
            run["goal"], run["run_id"] = e["goal"], e["run_id"]
        elif t == "task_declared":
            tasks[e["task_id"]] = {
                "goal": e["goal"], "agent_type": e["agent_type"],
                "status": "waiting" if e["parents"] else "ready",
                "summary": "", "error": "", "parents": e["parents"],
            }
        elif t == "task_status":
            tid = e["task_id"]
            if tid in tasks:
                tasks[tid]["status"] = e["to"]
        elif t == "result_published":
            tid = e["task_id"]
            if tid in tasks:
                tasks[tid]["summary"] = e["summary"]
                tasks[tid]["error"] = e["error"]
                tasks[tid]["status"] = e["status"]
    return run, tasks


def summarize_task(tid, task, audience, blockers=None):
    """One line per task, shaped by audience."""
    status = task["status"]
    icon = {"done": "OK", "failed": "FAIL", "claimed": "RUN", "ready": "QUEUED", "waiting": "BLOCKED"}.get(status, status.upper())
    line = f"[{icon}] {tid} ({task['agent_type']}): {task['goal']}"
    if status == "waiting" and blockers:
        line += f" — blocked by {','.join(blockers)}"
    if task["summary"] and audience in ("worker", "observer"):
        line += f" — {task['summary'][:MAX_SUMMARY]}"
    if task["error"] and audience in ("worker", "observer"):
        line += f" ERR: {task['error'][:120]}"
    return line


def build_digest(run, tasks, audience):
    """Audience-scoped digest. The trace operation: what does THIS receiver need?"""
    lines = [f"PIPELINE DIGEST [{audience}] — goal: {run['goal'][:80]}"]
    order = {"failed": 0, "waiting": 1, "ready": 2, "claimed": 3, "done": 4}
    visible = list(tasks.items())

    if audience == "orchestrator":  # full matrix — this is what the orchestrator itself sees
        lines.append(f"tasks={len(tasks)}")
        for tid, t in visible:
            lines.append(f"  {tid} {t['status']:8} parents={','.join(t['parents']) or '-'}")
        lines.append("full summaries in board categories")
    else:
        # audience shaping: receivers see what they can act on
        blocked_by = {}
        for tid, t in visible:
            if t["status"] == "waiting":
                failed_parents = [p for p in t["parents"] if p in tasks and tasks[p]["status"] == "failed"]
                blockers = failed_parents or t["parents"]
                blocked_by[tid] = blockers

        relevant = [x for x in visible if x[1]["status"] != "done" or x[1]["summary"]]
        lines.append(f"tasks={len(visible)}")
        if audience == "observer":
            # blockers + done-with-substance only; claimed/queued = transport noise
            relevant = [x for x in visible if x[1]["status"] in ("failed", "waiting") or x[1]["summary"]]
            lines.append(f"actionable={len([x for x in visible if x[1]['status'] in ('failed','waiting')])} done_with_findings={len([x for x in visible if x[1]['summary']])}")
        elif audience == "worker":
            relevant = [x for x in visible if x[1]["status"] in ("failed", "waiting", "ready", "claimed")]
            lines.append(f"your_queue={len([x for x in visible if x[1]['status'] in ('ready','claimed')])}")

        relevant.sort(key=lambda x: order.get(x[1]["status"], 9))
        for tid, t in relevant[:MAX_TASKS_IN_DIGEST]:
            lines.append("  " + summarize_task(tid, t, audience, blockers=blocked_by.get(tid)))

    digest = "\n".join(lines)
    # Budget policy: never truncate mid-line (silent amputation of payload).
    # Drop lowest-priority lines (done-with-findings are listed last) and note the drop.
    if len(digest) > MAX_CHARS:
        dropped_count = 0
        while len("\n".join(lines)) > MAX_CHARS and len(lines) > 2:
            dropped = lines.pop()
            if not dropped.startswith("  "):
                dropped_count -= 1  # structural line: put it back, report below
                lines.append(dropped)
                break
            dropped_count += 1
        digest = "\n".join(lines)
        digest += f"\n  [...{max(dropped_count, 1)} lower-priority line(s) dropped, full text on board]"
    return digest


def main():
    events = load_events(FIXTURE)
    run, tasks = reduce_stream(events)
    print(f"=== input: {len(events)} events ===")
    total = 0
    for audience in ("orchestrator", "worker", "observer"):
        d = build_digest(run, tasks, audience)
        total += len(d)
        print(f"\n--- digest [{audience}] ({len(d)} chars) ---")
        print(d)
    print(f"\n=== firehose: {sum(len(json.dumps(e)) for e in events)} chars raw -> {total} chars digested across 3 audiences ===")


if __name__ == "__main__":
    main()