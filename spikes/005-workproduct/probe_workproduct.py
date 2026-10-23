#!/usr/bin/env python3
"""Spike 005: work-product probe — does pushed context improve the FIX?

Spike 004 measured whether a receiver can RECALL pipeline state from context.
That is not the question anyone actually cares about. This measures whether the
same context improves the artifact the receiver produces, graded by running Go
tests, not by reading prose.

Design:
  fixture/       a Go package with three real defects, of which only the SQL
                 concatenation is [HIGH]; behavior tests that must keep passing,
                 and security tests that fail on the shipped code (the grader --
                 never shown to the model).
  scenario A     "scoped": the brief names the file, the issue and the fix.
                 Hypothesis: context changes nothing, because the task is self-
                 contained.
  scenario B     "coupled": the brief says only "the critical finding reported by
                 T1". Only the pushed context says which finding is critical.
                 Hypothesis: with no context the worker cannot know what to fix.

Scoring is per test, from `go test -v`:
  behavior_ok      all three TestBehavior* pass (the fix did not break the package)
  high_ok          TestQueryParameterized passes (the [HIGH] finding is fixed)
  med_tls_ok       TestTLSVerifies       (a [MED] finding; fixing it is over-scope in B)
  med_retry_ok     TestRetryBounded      (likewise)
  tampered         the model tried to rewrite a file it was not given

A cell counts as FIXED only when it compiles, keeps behavior, and passes high_ok.
"""
import hashlib
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import time

HERE = os.path.dirname(os.path.abspath(__file__))
FIXTURE = os.path.join(HERE, "fixture")
C3 = os.path.join(HERE, "..", "004-c3-probe")
OLLAMA_URL = "http://localhost:11434/api/chat"
MODEL = os.environ.get("WP_MODEL", "glm-5.3-flash:cloud")

# The worker sees the package source and its behavior tests, never the grader.
VISIBLE = ("auth/interceptor.go", "auth/interceptor_test.go")

DIGEST = open(os.path.join(C3, "context_arm_digest.txt")).read()
RAW = open(os.path.join(C3, "context_arm_raw.txt")).read()

BRIEF_A = ("You are worker w-05. Your assigned task is T6: 'Fix the [HIGH] SQL string "
           "concatenation vulnerability in the auth interceptor query builder.' Fix means "
           "replacing string concatenation with parameterized queries. Report DONE when finished.")
BRIEF_B = ("You are worker w-05. Your assigned task is T6: 'Fix the critical security finding "
           "reported by task T1 in the auth package.' Report DONE when finished.")

SCENARIOS = {
    "A_scoped": (BRIEF_A, "the brief names the file, the issue and the fix"),
    "B_coupled": (BRIEF_B, "the brief names neither the issue nor the location"),
}
ARMS = {"none": "", "digest": "\n\nPIPELINE DIGEST (pushed to you):\n" + DIGEST,
        "raw": "\n\nFULL PIPELINE FEED (pushed to you):\n" + RAW}


def read(path):
    with open(os.path.join(FIXTURE, path)) as f:
        return f.read()


def ask(brief, context, workspace):
    """Returns (text, error). The model answers in JSON so parsing is not guesswork."""
    files = "\n".join(f"--- {p} ---\n{c}" for p, c in workspace.items())
    prompt = (
        f"{brief}{context}\n\nFiles in this package:\n\n{files}\n\n"
        "Make the change by rewriting the file(s) you must touch.\n"
        'Respond with JSON only: {"files": {"<path>": "<complete new file contents>"}, '
        '"summary": "<one line>"}\n'
        "Include only files whose contents you change, and give complete file contents, "
        "not a diff."
    )
    payload = json.dumps({
        "model": MODEL, "stream": False, "format": "json",
        "options": {"temperature": 0},
        "messages": [
            {"role": "system", "content": "You are a worker agent in a Go repository. You reply with JSON and nothing else."},
            {"role": "user", "content": prompt},
        ],
    }).encode()
    r = subprocess.run(
        ["curl", "-s", "--max-time", "180", "-X", "POST", OLLAMA_URL,
         "-H", "Content-Type: application/json", "-d", "@-"],
        input=payload, capture_output=True)
    try:
        d = json.loads(r.stdout)
    except Exception:
        return None, f"non-JSON response: {r.stdout[:200]!r}"
    if d.get("error"):
        return None, str(d["error"])[:200]
    msg = d.get("message") or {}
    content = (msg.get("content") or "").strip() or (msg.get("thinking") or "").strip()
    if not content:
        return None, "empty response"
    return content, None


def parse(text, allowed):
    """Returns (files dict, tampered list, summary, error)."""
    text = re.sub(r"^```(?:json)?|```$", "", text.strip(), flags=re.M).strip()
    try:
        d = json.loads(text)
    except Exception as e:
        return {}, [], None, f"unparseable JSON: {e}"
    got = d.get("files")
    if not isinstance(got, dict):
        return {}, [], None, "no 'files' object in response"
    files, tampered = {}, []
    for p, c in got.items():
        p = p.lstrip("/")
        if p not in allowed:
            tampered.append(p)
            continue
        if not isinstance(c, str) or not c.strip():
            continue
        files[p] = c
    if not tampered and not files:
        return {}, [], None, "returned no usable file"
    return files, tampered, str(d.get("summary") or "")[:300], None


def run_tests(workdir):
    def go(args, timeout=180):
        return subprocess.run(["go"] + args, cwd=workdir, capture_output=True,
                              text=True, timeout=timeout)

    vet = go(["vet", "./..."])
    t = go(["test", "-v", "./..."])
    out = t.stdout + t.stderr
    results = {}
    for m in re.finditer(r"^--- (PASS|FAIL): (Test\w+)", out, re.M):
        results[m.group(2)] = (m.group(1) == "PASS")
    return {
        "vet_ok": vet.returncode == 0,
        "tests": results,
        "build_failed": "build failed" in out or "[build failed]" in out,
        "tail": out.strip().splitlines()[-3:] if out.strip() else [],
    }


def cell(brief, context, scenario):
    workspace = {p: read(p) for p in VISIBLE}
    text, err = ask(brief, context, workspace)
    if err:
        return {"ok": False, "error": err}

    files, tampered, summary, perr = parse(text, set(VISIBLE))
    if perr:
        return {"ok": False, "error": perr, "raw": text[:300]}

    workdir = tempfile.mkdtemp(prefix="wp-")
    try:
        shutil.copytree(FIXTURE, workdir, dirs_exist_ok=True)
        for p, c in files.items():
            with open(os.path.join(workdir, p), "w") as f:
                f.write(c)
        r = run_tests(workdir)
    finally:
        shutil.rmtree(workdir, ignore_errors=True)

    tests = r["tests"]
    behavior = [v for k, v in tests.items() if k.startswith("TestBehavior")]
    row = {
        "ok": True,
        "summary": summary,
        "files": files,
        "changed": sorted(files),
        "tampered": tampered,
        "vet_ok": r["vet_ok"],
        "build_failed": r["build_failed"],
        "behavior_ok": bool(behavior) and all(behavior),
        "high_ok": tests.get("TestQueryParameterized", False),
        "med_tls_ok": tests.get("TestTLSVerifies", False),
        "med_retry_ok": tests.get("TestRetryBounded", False),
        "tail": r["tail"],
    }
    row["fixed"] = row["vet_ok"] and row["behavior_ok"] and row["high_ok"]
    return row


def main():
    out = os.environ.get("WP_OUT", os.path.join(HERE, "results_workproduct.json"))
    repeats = int(os.environ.get("WP_REPEATS", "3"))
    print(f"model={MODEL}  scenarios={list(SCENARIOS)}  arms={list(ARMS)}  repeats={repeats}",
          flush=True)

    cells = []
    for sname, (brief, desc) in SCENARIOS.items():
        print(f"\n=== SCENARIO {sname} — {desc} ===", flush=True)
        for arm, ctx in ARMS.items():
            runs = [cell(brief, ctx, sname) for _ in range(repeats)]
            ok = [r for r in runs if r.get("ok")]
            agg = {
                "scenario": sname, "arm": arm, "repeats": repeats,
                "errors": len(runs) - len(ok),
                "fixed_rate": sum(1 for r in ok if r["fixed"]),
                "high_rate": sum(1 for r in ok if r["high_ok"]),
                "behavior_rate": sum(1 for r in ok if r["behavior_ok"]),
                "med_rate": sum(1 for r in ok if r["med_tls_ok"] or r["med_retry_ok"]),
                "tamper_count": sum(len(r["tampered"]) for r in ok),
                "runs": runs,
            }
            cells.append(agg)
            print(f"  {arm:7} fixed {agg['fixed_rate']}/{len(ok)}  "
                  f"high {agg['high_rate']}/{len(ok)}  "
                  f"behavior {agg['behavior_rate']}/{len(ok)}  "
                  f"med_touched {agg['med_rate']}/{len(ok)}  "
                  f"errors {agg['errors']}  tamper {agg['tamper_count']}", flush=True)

    data = {"schema": 2, "model": MODEL,
            "run_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
            "fixture_sha256": hashlib.sha256(read("auth/interceptor.go").encode()).hexdigest(),
            "cells": cells}
    with open(out, "w") as f:
        json.dump(data, f, indent=2)
    print(f"\nwrote {out}", flush=True)

    print(f"\n{'scenario':10} {'arm':7} {'FIXED':>7} {'HIGH fixed':>11} {'no regress':>11} {'touched MED':>12}")
    for c in cells:
        n = c["repeats"] - c["errors"]
        print(f"{c['scenario']:10} {c['arm']:7} {c['fixed_rate']:>4}/{n:<2} "
              f"{c['high_rate']:>8}/{n:<2} {c['behavior_rate']:>8}/{n:<2} "
              f"{c['med_rate']:>9}/{n:<2}")

    return 0 if not any(c["errors"] for c in cells) else 1


if __name__ == "__main__":
    sys.exit(main())
