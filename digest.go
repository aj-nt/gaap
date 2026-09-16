package gaap

// digest.go: audience-scoped pipeline digests (the "trace-emitter" layer).
// The orchestrator holds the DAG matrix; receivers see traces of it.
// Spike evidence (spikes/001-003): templated digests carried all actionable
// state at ~26% of raw context; cost scales with findings, not task count.
// Rules baked in from the spike:
//   - transport noise (heartbeats, leases) contributes zero bytes by construction
//   - never truncate mid-line: drop whole lowest-priority lines, declare the drop
//   - blockers are named before findings are lost (attribution > content)

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// digestMaxChars is the per-audience digest budget. 900 = spike 003's knee
// for a findings-bearing fixture; overridable in tests.
var digestMaxChars = 900

const digestMaxSummary = 160
const digestMaxTasks = 12

// buildDigest renders the current DAG state as a digest for one audience.
// Pure function of (DAG state, audience, budget) — deterministic, no LLM.
func (o *Orchestrator) buildDigest(audience string) string {
	lines := []string{fmt.Sprintf("PIPELINE DIGEST [%s] — goal: %.80s", audience, o.goal)}

	all := collectDigestTasks(o)
	lines = append(lines, fmt.Sprintf("tasks=%d", len(all)))

	blockedBy := map[string][]string{}
	for _, t := range all {
		if t.status == "blocked" {
			var failedParents, blockers []string
			for _, p := range t.parents {
				if pn, ok := o.dag.nodes[p]; ok {
					if pn.Status == "failed" {
						failedParents = append(failedParents, p)
					} else {
						blockers = append(blockers, p)
					}
				}
			}
			if len(failedParents) > 0 {
				blockedBy[t.id] = failedParents
			} else {
				blockedBy[t.id] = blockers
			}
		}
	}

	rank := map[string]int{"failed": 0, "blocked": 1, "ready": 2, "claimed": 3, "done": 4}
	var relevant []digestTask
	switch audience {
	case "orchestrator":
		for _, t := range all {
			lines = append(lines, fmt.Sprintf("  %s %-8s parents=%s", t.id, t.status, strings.Join(t.parents, ",")))
		}
		lines = append(lines, "full summaries in board categories")
	case "worker":
		for _, t := range all {
			if t.status != "done" {
				relevant = append(relevant, t)
			}
		}
	case "observer":
		for _, t := range all {
			if t.status == "failed" || t.status == "blocked" || t.summary != "" {
				relevant = append(relevant, t)
			}
		}
	}

	if audience != "orchestrator" {
		if anoms := breakerAnomalies(o); anoms != "" {
			lines = append(lines, anoms)
		}
		sortDigestTasks(relevant, rank)
		if len(relevant) > digestMaxTasks {
			relevant = relevant[:digestMaxTasks]
		}
		for _, t := range relevant {
			lines = append(lines, "  "+digestTaskLine(t, blockedBy[t.id]))
		}
	}

	return enforceDigestBudget(lines, digestMaxChars)
}

// digestTask is one task's projection into a digest line.
type digestTask struct {
	id, status, goal, summary, agentType string
	parents                              []string
}

func collectDigestTasks(o *Orchestrator) []digestTask {
	var all []digestTask
	for id, node := range o.dag.nodes {
		all = append(all, digestTask{
			id: id, status: node.Status, goal: node.Goal,
			summary: node.Summary, agentType: node.AgentType, parents: node.ParentIDs,
		})
	}
	return all
}

func digestTaskLine(t digestTask, blockers []string) string {
	icon := map[string]string{"done": "OK", "failed": "FAIL", "claimed": "RUN", "ready": "QUEUED", "blocked": "BLOCKED"}[t.status]
	if icon == "" {
		icon = strings.ToUpper(t.status)
	}
	line := fmt.Sprintf("[%s] %s (%s): %s", icon, t.id, t.agentType, t.goal)
	if t.status == "blocked" && len(blockers) > 0 {
		line += fmt.Sprintf(" — blocked by %s", strings.Join(blockers, ","))
	}
	if t.summary != "" && t.status != "blocked" {
		line += fmt.Sprintf(" — %.160s", t.summary)
	}
	return line
}

// enforceDigestBudget drops whole lowest-priority trailing lines (never the
// header) until under budget, then declares the drop. Never truncates mid-line.
func enforceDigestBudget(lines []string, max int) string {
	if strings.Join(lines, "\n") == "" {
		return ""
	}
	joined := strings.Join(lines, "\n")
	if len(joined) <= max {
		return joined
	}
	dropped := 0
	for len(lines) > 2 && len(strings.Join(lines, "\n")) > max {
		last := lines[len(lines)-1]
		if !strings.HasPrefix(last, "  ") {
			break // structural line: never drop
		}
		lines = lines[:len(lines)-1]
		dropped++
	}
	out := strings.Join(lines, "\n")
	if dropped > 0 {
		out += fmt.Sprintf("\n  [...%d lower-priority line(s) dropped, full text on board]", dropped)
	}
	return out
}

func sortDigestTasks(tasks []digestTask, rank map[string]int) {
	for i := 1; i < len(tasks); i++ {
		for j := i; j > 0 && rank[tasks[j].status] < rank[tasks[j-1].status]; j-- {
			tasks[j], tasks[j-1] = tasks[j-1], tasks[j]
		}
	}
}

// emitDigests publishes audience-scoped digests to the push channel after a
// DAG advancement. Best-effort: publish failures never block DAG advancement.
// This is the trace-emitter seam: the orchestrator alone holds the matrix;
// receivers get traces, shaped per audience. Full-fidelity results remain on
// the board (task_result category) for receivers that can poll.
func (o *Orchestrator) emitDigests(ctx context.Context) {
	if o.daemon == nil {
		return
	}
	runKey := o.RunKey()
	for _, audience := range []string{"observer", "worker"} {
		digest := o.buildDigest(audience)
		key := fmt.Sprintf("digest_%s_%s_%d", runKey, audience, time.Now().UnixNano())
		if _, err := o.daemon.AddMemory(ctx, "push", "pipeline_digest", key,
			digest, 4, "gaap-orchestrator"); err != nil {
			slog.Warn("digest publish failed (non-fatal)", "audience", audience, "error", err)
		}
	}
}

// breakerAnomalies renders non-closed circuit breakers as one anomalies line.
// Spike 002's Q10 gap: breaker/lease anomaly state was the only content
// exclusive to the raw firehose — this template block closes it.
func breakerAnomalies(o *Orchestrator) string {
	var parts []string
	for agentType, cb := range o.breakerRegistry {
		if st := cb.State(); st != StateClosed {
			parts = append(parts, fmt.Sprintf("%s breaker %s (%d failures)", agentType, st, cb.FailureCount()))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "ANOMALIES: " + strings.Join(parts, "; ")
}
