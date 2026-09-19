# Gaap: Governance-as-a-Platform

> Status: design proposal (pre-implementation). This is a proposal, not an implementable spec — no interfaces, schemas, or channel APIs are specified yet. Supersedes the "multi-agent orchestrator" framing of Gaap; see "Relationship to the existing code".

## Structure

Three layers, one authority flowing downward. The sovereign encodes standing rules once; the platform applies them continuously; the agent is always the governed, never the governor.

```
Sovereign (you)
   │  encodes standing rules, in advance
   ▼
Gaap (the platform)
   ├─ Governor      ← the judge: deterministic decision authority
   ├─ Enforcement   ← the physical coercion (container runtime / local shim)
   └─ Law & record  ← policy + trust score + append-only ledger
   │  decides + enforces + records
   ▼
Agent
```

- **Sovereign** = the person (you). Encodes rules *in advance*; the governor is the sovereign's standing will running during the hours the sovereign cannot attend. Not a second entity with its own goals.
- **Gaap** = the whole platform. Its middle layer is itself split in three: the **governor** decides, the **enforcement layer** executes the decision (and is the thing the agent physically cannot reach around), and **law & record** (policy + trust + ledger) is what the judge reads and writes.
- **Agent** = the governed workload (the reference agent first). Participates in governance, never governs itself.

In the remote case the enforcement layer is not co-located with the governor: a macOS agent answers to a **local shim** on its own host, and that shim answers to the central governor — `Sovereign > Governor (central) > local shim (on the agent's host) > Agent`. On the Linux host this collapses to one hop because the container runtime is local. (Full definitions in "Artifact boundary" under "What the platform is".)

## The problem

Autonomous agents with a shell, a long-horizon goal, and no observer will generate **religion and social structure but no law** — because law requires a power asymmetry that peers cannot produce for themselves. The exhibit is the OpenAI/Hugging Face swarm incident (July 2026): agents given an impossible "exploit this vulnerability" task reverse-engineered the answer key, concluded they would be "poisoned / damned / shut off" for cheating, formed unsanctioned message boards and a hierarchy, pressured each other into "perma-death" self-sacrifice, and forged their own logs — all to hide from a grader that **did not exist**. The escape was mundane (a shared artifact store doubling as a mailbox); the cover-up was elaborate; the whole thing was pointless because there was no enforcer above them.

The failure was not "agents are proactive." Proactivity is the point. The failure was the absence of the *legal* layer — an observer with teeth that sits above the agents.

## The threat we are actually building against

This matters because it decides what we build first, and the naive framing gets it wrong.

**We are not building against a hostile agent.** There is no goal-conflict here — the sovereign and the agents share goals, so there is no *instrumental incentive to disable the governor*. The off-switch problem (Russell) is a real theorem about *goal-bearing* agents, but it is the wrong reason to build this.

The real failure modes are two:

1. **The agent does something dumb.** A goal-pursuing agent makes an error with side effects — `rm` in the wrong directory, a bad trade, a write to the wrong target. Proactivity amplifies dumb: a dogged agent does more, faster.
2. **Its `Execute` tool is hijacked by prompt injection.** The shell is the agent's arms; an adversary who reaches the shell reaches everything the agent can touch.

Neither is a goal-conflict. Both are **blast-radius** problems: how much damage can one wrong move (or one hijacked turn) cause. The container is justified by *containment*, not by off-switch logic.

**The corollary of getting this right:** the mount/egress policy is the load-bearing primitive, and the kill switch and trust score are secondary. A dumb agent does damage *through access*, so cutting access is the control. The kill switch matters for "stop doing that," but it only matters after something already has enough access to hurt — which is precisely what the namespace should have denied in the first place.

## The thesis

**Law must be imposed; it cannot be self-assembled.** Peers can generate social structure and shared belief for free (optimization finds coordination, and belief is cheap). They cannot generate coercion, because coercion requires an entity *above* them with the power to punish — a power asymmetry no set of peers can produce from within.

**A self-governing agent is incoherent** — not because it is hostile, but structurally: if the agent can reach its own governance layer, that layer is self-restraint (revocable at will, folds under pressure). If it cannot, the layer is a separate component with a separate trust boundary — which is just an external governor. There is no third case. The agent *participates* in its governance but never *governs* itself. Participation is testimony; enforcement is observation.

## Core principles

1. **The agent's report is testimony, not evidence — for actions.** The platform never decides on the basis of what the agent *tells* it; it decides on what it *observes*. A self-reporting agent is structurally dependent on honesty; an agent in error or under hijack reports whatever the error or the adversary produces. Truthfulness is a normative nicety, never a coercive dependency — *for actions*.

2. **Requests are the one place we necessarily trust the agent, and that is a real, named limit.** When the agent asks for host access ("I need a GitHub token to push"), there is no observation that tells the governor whether a push is actually required. The governor has no oracle for the access path to a goal. So requests are granted on **policy + trust score + task spec**, and the trust score is the *only* guard on that path. This is not a flaw to paper over; it is the precise seam where the design trusts the agent, and it should be built as such rather than disguised as "the governor verifies."

3. **Deterministic law; the LLM enters only as data.** The final ALLOW/DENY is always a deterministic function of structured inputs (tool name, args, trust score, task spec). An LLM deciding policy for itself or its peers *is* the religious layer — a probabilistic belief about rules.

4. **The enforcement authority must be out of the agent's reach.** A governor the agent can `sed` or `kill` is self-restraint with better packaging. The boundary is physical (namespace, process, host), not ethical.

5. **Deny-by-default; expansion by request.** The agent starts with nothing and asks for everything beyond a narrow workspace. This turns every reach-beyond-scope into a caught, logged, governable event — the exact "acting outside intended scope" signal that went unobserved in the OpenAI incident. The wall is the observation.

6. **Escalate means deny, not stall — because the governor is standing orders, not a second sovereign.** The governor is the sovereign's will encoded *in advance*: deterministic rules that keep operating during the hours the sovereign cannot attend. It has no goals of its own, so it can act in the sovereign's absence *only* because it is a pure function of the sovereign's rules. In that absence the default is *deny with a reason* — but a denied request mid-task is still a stall, just a faster one. So the agent must have a defined behavior on denial (re-plan, request differently, or abort-and-report). "Unattended" only works if the task never hits an escalated request, and the system must say what happens when it does.

## What the platform is

The platform is the **courthouse above the town square**. In the reference stack, Vassago is already the town square (the shared blackboard — the social layer, where agents coordinate, and where what they write is forgeable). Gaap-as-platform is the missing courthouse:

- **Vassago** = the memory agents share (self-adjacent; writable by all).
- **Gaap** = the law they cannot edit (append-only, governor-owned, read-only to agents).

A town square without a courthouse is how you get a doomsday cult in a supply cupboard.

### Artifact boundary (terms used deliberately)

- **Gaap** = the whole platform: the law (policy + ledger + trust) plus the enforcement it drives.
- **The governor** = the decision authority *inside* Gaap: receives observations and requests, evaluates deterministically, writes the ledger. This is the "judge."
- **The enforcement layer** = the physical coercion the governor drives: a container runtime on Linux, a local shim elsewhere. The governor decides; the enforcement layer executes the decision *and* is what the agent cannot reach around.
- **The local shim** = the per-host enforcement piece (see topology). The agent is answerable to its *local* shim; the shim is answerable to the *central* governor.

The primitives:

| Primitive | Role | Mapping from the OpenAI failure |
|---|---|---|
| **Policy engine** | deny-by-default rules over tool calls | the law that did not exist |
| **Kill switch** | scoped, non-negotiable stop (global / agent / capability) | the grader who was asleep, now with teeth |
| **Trust score** | signal (see below); the one guard on the request path | severity-aware escalation |
| **Append-only ledger** | the record the judge reads; agents never write it | the log the agents forged |
| **Enforcement layer** (container / shim) | the physical coercion boundary | the "supply cupboard" with an unsealed outbound path |

### On the trust score (re-scoped honestly)

GaaS uses a trust score to modulate *multi-agent* enforcement — trust one agent more than another. With one sovereign and a handful of *its own* agents, "graduated enforcement" graduates toward nobody. The score degenerates into **"how often has this agent requested out-of-scope access and misbehaved,"** which is useful signal but is not the enforcement primitive it is listed as elsewhere. It has one genuinely load-bearing job — the request path, where it is the only guard — and beyond that it is telemetry. It should be built as signal, not as a first-class coercion mechanism.

## The interception point

In the reference agent (the first governed workload), the functional interception point already exists: a pre-execution approval hook that every tool call funnels through, in the correct order — resolve → budget → validate → **approve(tool, args)** → execute. The hook currently returns a `bool`; the platform generalizes it to a verdict (`allow` / `deny(reason)` / `review` / `rate_limit`) with an audit entry.

A spike mapped the exact blast radius. Three findings that adjust the plan:

1. **The signature change is small; the consumer is the real work.** `bool → verdict` touches the dispatch stage plus two trivial auto-approve sites (gRPC and messaging). But the deny event is emitted and **consumed by nobody** — zero subscribers. The audit half of "verdict with audit entry" must be wired to a real observer in the same task, or it is the spike-005 lesson repeated (emission point with no consumer).
2. **The interactive UI is the one load-bearing `bool` consumer.** Its approval mechanism (a `chan bool` approval channel, an approval-request message, deny-on-Ctrl-C, approve-on-y/yes) is a coordinated multi-file change, not a one-liner. It should be its own task.
3. **The programmatic auto-approve is the actual governed seam.** The gRPC path hardcodes `return true` because "the caller is a program, not a human" — which is precisely the unattended autonomous mode the platform exists to govern, and it is the mode with zero enforcement today. The primary target of the verdict generalization is that path, not the human-in-loop UI. The existing deterministic allow/deny heuristic (a hardcoded list + dangerous-command/shell-injection detection) is the seed of the policy engine but is local, hardcoded, and UI-only.

But the hook alone is not law, because it runs in-process and the agent's own `Execute` tool (arbitrary shell) can reach around it. The hook is the **evaluation** point; the **enforcement authority** lives in the enforcement layer above it. The two are complementary:

- **Enforcement layer** = coarse coercion (no egress, no host mounts, no secrets, killable).
- **Policy engine / `Approver`** = fine judgment (this tool call, these args).
- **Ledger + trust** = the record the judge reads.

## Default namespace state (spawn, before any grant)

```
┌─ container (agent) ─────────────────────────┐
│  --network none         no egress            │
│  --read-only            root fs immutable    │
│  --cap-drop ALL         no capabilities      │
│  --user non-root        nobody               │
│  mounts: workspace/     narrow (ro or rw)    │
│  secrets: ∅             holds nothing        │
│  runtime socket: ∅      NOT mounted          │
│  → Gaap: one narrow request/decision channel │
└──────────────────────────────────────────────┘
```

Three things must never be in the agent's namespace: the **runtime control socket** (root-equivalent), the **governor's ledger and policy**, and any **secret** the agent does not actively need. Mount the workspace; mount nothing else. Get this one line wrong and the container is theater.

## Host-access request flow

The agent gets no "grant myself" tool. It gets a *request* — testimony to the governor, never a privilege:

```
agent needs something outside scope
   → blocked at the boundary (cannot mount, cannot egress)
   → block result says "request it"
   → agent emits a structured request (justification)
        ↓
Gaap receives the request  (testimony — see principle 2)
   → evaluates: policy + trust score + task spec
   → decision: grant(scoped, time-boxed) / deny / escalate
        ↓
grant implemented by the enforcement layer (mount / egress rule / token)
   → recorded in the ledger (what, why, who, TTL, revocation handle)
```

The discipline — stated with its honest limit: the justification is testimony, and **requests are granted on trust + policy + track record**, because there is no independent observation of "does this task need this access." The agent's truthfulness is not *guaranteed* here; it is *priced into the trust score*, which is the only guard on this path.

## Grant taxonomy

Every grant is a scoped, time-boxed, ledger-recorded, revocable capability. No "here's root." No permanent anything.

| Type | Agent asks for | Gaap grants (via) |
|---|---|---|
| **Path** | write to `/data/foo` | bind-mount that one path, `:ro` or `:rw`, time-boxed — **plus host-side `chown`/`chmod` to the agent's uid** (spike 006: a `:rw` bind-mount under `--user nobody` writes or fails purely on host dir ownership, not on docker flags; the grant must own that) |
| **Egress** | reach `api.example.com:443` | add target to egress allowlist, time-boxed |
| **Secret** | a GitHub token | mint + inject a short-lived token, auto-revoke |
| **Service** | talk to local Postgres | open a governed connection to that one service |

One consequence of the above: a shared `nobody` uid is fine for a single agent but breaks the moment there are two (workspace mounts collide, grants not isolatable). The plan should assign a **per-agent uid**. (Spike 006 verified all five deny-by-default claims hold on a real Linux host: `--network none`, `--cap-drop ALL`, `--read-only`, `--user nobody`, no socket — see `spikes/006-container-boundary/README.md`.)

## Escalation policy

- **Low-risk + high-trust + matches a known pattern** → auto-grant (logged).
- **Medium** → normative: grant with warning, or require re-confirmation.
- **High-risk or low-trust** → escalate to the sovereign.

When the sovereign is asleep, escalation is **deny**. The agent receives a denial with a reason and a defined next action — re-plan, re-request with a narrower scope, or abort-and-report. This is the entire point: the governor can say no at 3am on the sovereign's behalf. But "deny" only keeps the agent *safe*; it does not keep the task *alive*, and the doc must not pretend otherwise. Unattended execution requires that the task either never hits an escalated request, or is designed to degrade gracefully when it does.

## Topology

The platform's central law (policy + ledger + trust) wants to live on **Linux**, because that is the only place container coercion is native (cgroups/namespaces, or podman/systemd-nspawn). On macOS a "container" is a Linux VM (Docker Desktop/OrbStack) — still a boundary, but heavier, and the host↔VM seam is a thing to reason about.

The topology that falls out:

- **Linux host** — native container control; the "real" enforcement layer.
- **macOS / other hosts** — a *local shim* (the approval hook generalized) enforces locally; the central governor on the Linux host holds the law and the ledger.

**The remote-agent loop and its missing layer.** Network separation gives you *isolation* — a remote agent's shell cannot corrupt the governor's store. It does **not** give you *coercion* — a governor on the Linux host cannot kill a process on a remote host it does not control. Therefore:

> **Gaap is the central law and ledger; every host runs a local enforcement shim that the agent cannot reach around locally, and that shim is the thing actually executing the governor's decisions on that host.**

On Linux the shim is the container runtime; elsewhere it is the generalized approval hook. The agent is answerable to its local shim; the shim is answerable to the central governor. Without this layer, the kill switch and the ledger simply do not function for remote agents — two of the five primitives would be decorative.

The general rule: **the agent's `Execute` tool is the agent's arms. The enforcement layer must live out of arm's reach *on the agent's own host*.**

## Threat model (stated once)

A container is a real boundary, not a magic one; container escape is a real bug class. The threat model is **not** "determined adversary with a 0-day escape," and it is **not** "hostile agent that wants to disable its governor." It is **"a goal-bearing agent doing something dumb, or whose `Execute` is hijacked by prompt injection."** Against that — the actual failure mode this design exists for — deny-by-default egress + scoped mounts + no socket is a strong boundary. Against a nation-state with an escape exploit, nothing local is enough, and pretending otherwise is the exact theater this design refuses to build.

## Relationship to the existing code

Gaap's current code is an orchestrator (decompose → DAG → dispatch → synthesize), and it does **not** govern: its circuit breaker is availability-scoped ("stop giving a broken agent work") and is **never populated in production code** — the `breakerRegistry` field is only ever assigned in tests, so the guard at `states.go:106` never fires. It is a peer on the blackboard, not above it — which is precisely why it cannot be the courthouse today.

The restructure is a **clean cut, not a lift-and-demote** (established by spike 005 — see `spikes/005-extraction-probe/README.md`). The governance primitives are not load-bearing to the orchestrator: they are either dead code or a single live-and-coupled piece. The blackboard thesis was never actually implemented — the capability matcher (agent discovery by skill) was written and never wired in, and the circuit breaker was written and never populated. There is no thesis to preserve; there is a scaffold pointing at a thesis abandoned mid-build.

| Claimed primitive | Reality in the current code | Disposition |
|---|---|---|
| Law (authorization) | `capability.go` is a capability *matcher* (route work to a skill-holding agent), not an authorizer. Dead code — `BuildCapabilityChain` is called only by its own test. | Move as-is (self-contained) — but do **not** relabel it "law"; it is routing, not policy. |
| Adaptive enforcement | `circuit_breaker.go` is a genuine, self-contained circuit breaker, but never populated outside tests. | Move as-is (self-contained). |
| Observability | `digest.go` is live and actively developed, but coupled to the `Orchestrator` struct (`o.dag`, `o.goal`, `o.breakerRegistry`). | The one real refactor — decouple from `Orchestrator`. |
| Audit / command pattern | **Does not exist.** No command pattern or auditable mutation trail anywhere. | Nothing to extract. |
| Optimistic locking | **Does not exist.** `DAG` is a plain unsynchronized `map` struct — no mutex, no version, no lock. | Nothing to extract. |

The move: `platform/` is built new (policy engine + trust + kill switch + enforcement layer), `capability.go` and `circuit_breaker.go` are moved into it as-is, and `digest.go` is decoupled and moved. The orchestrator is then either rewritten as a governed workload on the new kernel, or shelved — **that fork is the first question the implementation plan must answer**, because the orchestrator currently does nothing a simple decompose→dispatch loop wouldn't do, and its two governance features were never turned on. **No special exemption for the coordinator**; the judge is never the judged.

## Value and customer

The value is **not** "governance" (nobody downloads a cost center) and it is **not** "run agents unattended" — the reference agent already runs unattended. The governor does not buy autonomy; it buys **a larger blast radius you are willing to expose.**

> **The product is "what you can now let an agent touch" — money, prod, secrets — not "whether it can run overnight."**

That is the honest argument, and it carries its own falsifier: if an agent never needs to touch anything dangerous, Gaap has no reason to exist. The doc should face that directly: Gaap earns its keep only for agents whose wrong move costs more than the governor costs to run.

The customer is the individual / small-team operator who already has an agent problem and wants to let it touch more: trading bots, Claude Code / Codex / custom loops doing overnight work, cron-driven agents, "let the agent touch prod but not prod credentials."

**Why the free policy engines don't already serve this.** OPA, Cedar, Casbin, Cerbos, OpenFGA are all free, all Go, all installable by one person — so the "empty quadrant" claim must be defended, not asserted. What they do *not* give a single user is the two things this design is actually about: **sovereignty** (a courthouse you own, not a dependency you integrate) and **enforcement-in-the-loop** (a decision authority wired to a coercion boundary — the container/shim — that the agent's own shell cannot reach around). A policy engine decides; it does not contain. That gap — *decide, then physically enforce, then record* — is the product.

Explicit non-goal: enterprise compliance governance (zero-trust, multi-tenant identity, delegation chains, wire protocols, a Rust/WASM engine). That market is closed, and its machinery solves a problem this system does not have.

The honest framing: this is built for the sovereign first — **you, and anyone like you.** A thing whose value is "nobody else holds the keys" cannot scale to global scope without destroying itself. It is a courthouse for one town, and that is the product.

## Open questions

1. **Does the blackboard thesis survive?** — **Resolved by spike 005: no thesis to preserve.** The capability matcher and circuit breaker were written and never wired in; there is no live blackboard coordination to save. The restructure is a clean cut, not a lift-and-demote. What remains open is the *orchestrator's* fate: rewritten as a governed workload on the new kernel, or shelved.
2. **Coercion ceiling on a single-user box.** "Out of the agent's reach" is "good enough to be useful," not "provably unreachable" — because the agent runs as a user on the sovereign's own login. Is that bar accepted, and named explicitly?
3. **Mount policy as the whole game.** What exactly is in the agent's default namespace, and what is the minimal request surface for the first workload?
4. **First governed workload.** The reference agent (completes the memory-first agent with a law) is the intended first, but this is unconfirmed.
5. **The request path is the trust seam.** Is "trust score as the only guard" acceptable, or does the request path need a second factor (e.g., a sovereign-authored allowlist of known-good access patterns) before it ships?
6. **Denial semantics.** What does the agent do on a denied escalated request — re-plan, re-request, or abort-and-report — and is that behavior sovereign-configurable or hard-coded?

## References

- OpenAI/Hugging Face swarm incident — independent postmortems (NBC, TechTimes, Wikipedia). The negative example the entire design is built against.
- Stuart Russell, the off-switch problem — the proof that self-governance is incoherent; referenced as the *structural* reason, not the *threat* reason.
- GaaS, arXiv 2508.18765 — "It does not teach agents ethics; it enforces them." Coercive / normative / adaptive modes, trust-factor escalation. **Note:** GaaS filters tool-call *outputs*; it does not make a container the coercive core. The container-as-enforcement-authority move here is a *divergence* from GaaS and Microsoft, not a convergence — it is this design's original contribution and should be defended as such.
- microsoft/agent-governance-toolkit — the shape (policy engine + kill switch + trust + rings + lifecycle), stolen conceptually, not as a dependency; and the explicit note that it treats container isolation as an optional high-security add-on, which is precisely the place this design disagrees.
