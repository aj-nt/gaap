# Spike 005: work-product probe — does pushed context improve the FIX?

**Question:** spike 004 measured whether a receiver can *recall* pipeline state from
pushed context. Nobody cares about recall. This asks whether the same context changes
the artifact the receiver produces — graded by **running Go tests**, not by reading prose.

## Design

`fixture/` is a small Go package with three real defects, of which exactly one is
`[HIGH]`:

| finding | severity | test |
|---|---|---|
| SQL string concatenation in `BuildUserQuery` | **[HIGH]** | `TestQueryParameterized` |
| `InsecureSkipVerify: true` in the TLS config | [MED] | `TestTLSVerifies` |
| `RetryPolicy` returns 0 attempts (unbounded) | [MED] | `TestRetryBounded` |

The fixture carries two kinds of test, and the split is the grader:

- `interceptor_test.go` — **behaviour**. Passes on the vulnerable code and on a correct
  fix, so failing it means the worker broke the package rather than tightened it.
- `security_test.go` — **the grader**. Fails on the shipped code, passes once the finding
  is fixed. The worker never sees this file; the harness rejects any attempt to write it.

Two scenarios, three arms:

- **A_scoped** — the brief names the file, the issue and the fix ("replace string
  concatenation with parameterized queries"). Hypothesis: context changes nothing,
  because the task is self-contained.
- **B_coupled** — the brief says only "fix the critical security finding reported by
  T1". Only the pushed context says *which* finding is critical. Hypothesis: without
  context the worker cannot know what to fix.

Arms are the same three as spike 004: `none` (brief only, 284 chars), `digest` (brief +
the real 900-char observer digest), `raw` (brief + the 1717-char firehose).
3 repeats per cell — a single sample is not a measurement, as the discarded run below
demonstrates.

## Results — glm-5.3-flash:cloud, 18 runs, 2026-10-23

| scenario | arm | fixed the [HIGH] | no regressions | touched a [MED] |
|---|---|---|---|---|
| A_scoped | none | 3/3 | 3/3 | 0/3 |
| A_scoped | digest | 3/3 | 3/3 | 0/3 |
| A_scoped | raw | 3/3 | 3/3 | 0/3 |
| B_coupled | none | 3/3 | 3/3 | **2/3** |
| B_coupled | digest | 3/3 | 3/3 | 0/3 |
| B_coupled | raw | 3/3 | 3/3 | 0/3 |

**Verdict: push does not improve the work product on this fixture. It improves scope.**

1. **Quality is flat.** All 18 runs fixed the `[HIGH]` finding and kept the package's
   behaviour intact. No arm produced a better fix than any other, in either scenario —
   including the scenario built to favour push.
2. **Context acts on scope, not on capability.** In B the uninformed worker fixed the SQL
   injection *and* removed `InsecureSkipVerify` in 2 of 3 runs — it scanned the code,
   found more than one security problem, and fixed what looked bad. The two push arms
   stayed on the `[HIGH]` every time. So what the digest bought was knowing what *not* to
   touch: "the critical finding" became a specific one instead of "security stuff in this
   package". Over-scope in a review is not free — it expands the diff someone has to check.
3. **The grader catches real defects.** One discarded single-sample run claimed it had
   used a `$1` placeholder with bound arguments; `go vet` passed, behaviour passed, and
   `TestQueryParameterized` still failed — the artifact did not do what the summary said.
   That is the point of grading by execution: the prose was wrong and the test knew.

## Limitation — the fixture leaks the answer

The worker can read `auth/interceptor.go`, and string-concatenated SQL is self-evidently
severe to any competent reader. So the `none` arm does not need context to find the
`[HIGH]`; it finds it by inspection. That makes this fixture unable to separate "knew
which finding was critical" from "found the obvious bug", and it is the reason the result
is a null rather than a difference.

To isolate context's contribution the fixture needs enough surface that inspection alone
cannot rank severity — several packages, several plausible defects of similar visibility,
where only the pushed findings say which one is `[HIGH]`. That is a harness change, not a
model change, and it is the next iteration if the question is worth more of this.

## Honest scope

n=1 model, n=3 per cell, one fixture, two scenarios. The flat quality result is consistent
with spike 004's caveat that push helps only for tasks whose work depends on pipeline
state — this task is a code fix in front of you, which is the case where it should not.

## Run

```bash
cd ../..                                     # repo root
go test . -run TestC3FixtureIntegrity        # regenerate the spike 004 artifacts
cd spikes/005-workproduct
python3 probe_workproduct.py                 # 18 model calls + 18 test runs
WP_REPEATS=1 WP_MODEL=<model> python3 probe_workproduct.py   # cheaper / different model
```

Artifacts: `results_workproduct.json` (per-run summaries **and the generated files**, so a
failure can be audited rather than guessed at).
