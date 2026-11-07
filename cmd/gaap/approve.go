// This file is part of Gaap.
// See LICENSE-Apache-2.0 for license information.

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aj-nt/gaap"
	"github.com/aj-nt/vassago-sdk/client"
	pb "github.com/aj-nt/vassago-sdk/proto"
)

// approvalClient is the slice of the daemon these two commands use. Narrow on
// purpose: it is the whole contract of answering a question, and it keeps the
// commands testable without a server.
type approvalClient interface {
	FindReadyTasks(ctx context.Context, agentType string, limit int32) ([]*client.TaskEntry, error)
	AddMemory(ctx context.Context, target, category, key, content string, priority int32, sourceAgent string) (*pb.MemoryEntry, error)
	CompleteTask(ctx context.Context, taskID, resultKey string) (*client.TaskEntry, error)
}

// pendingApproval is one question the daemon is holding for a human.
type pendingApproval struct {
	ApprovalTaskID string               `json:"approval_task"`
	Request        gaap.ApprovalRequest `json:"request"`
	RequestJSONErr string               `json:"request_error,omitempty"`
}

// row flattens the request for scripts: `--json` output is one array of tasks,
// not an array of envelopes. jq on the answer should not need two hops.
func (p pendingApproval) row() map[string]any {
	row := map[string]any{
		"approval_task": p.ApprovalTaskID,
		"task_id":       p.Request.TaskID,
		"agent_type":    p.Request.AgentType,
		"attempts":      p.Request.Attempts,
		"reason":        p.Request.Reason,
		"opened_at":     p.Request.OpenedAt,
		"deadline":      p.Request.Deadline,
	}
	if p.RequestJSONErr != "" {
		row["request_error"] = p.RequestJSONErr
	}
	return row
}

type approvalsConfig struct {
	RunID      string
	DaemonAddr string
	APIKey     string
	TLSCert    string
	JSON       bool
}

func (c *approvalsConfig) settings() llmSettings {
	return llmSettings{DaemonAddr: c.DaemonAddr, APIKey: c.APIKey, TLSCert: c.TLSCert}
}

// approvalsCommand lists the questions a run is waiting on.
func approvalsCommand(args []string, stdout io.Writer) error {
	cfg, err := parseApprovalsFlags(args)
	if err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()

	c, err := dialDaemon(ctx, cfg.settings())
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	return approvalsRun(ctx, c, cfg, stdout)
}

func parseApprovalsFlags(args []string) (*approvalsConfig, error) {
	cfg := &approvalsConfig{}
	fs := flag.NewFlagSet("approvals", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	fs.StringVar(&cfg.DaemonAddr, "addr", "", "Vassago daemon address (default: localhost:50051)")
	fs.StringVar(&cfg.APIKey, "api-key", "", "Vassago daemon API key (Bearer token)")
	fs.StringVar(&cfg.TLSCert, "tls-cert", "", "Path to TLS CA certificate for daemon connection")
	fs.BoolVar(&cfg.JSON, "json", false, "Print the requests as JSON")

	if err := fs.Parse(reorderArgs(args, map[string]bool{
		"addr": true, "api-key": true, "tls-cert": true,
	})); err != nil {
		return nil, err
	}
	if fs.NArg() != 1 {
		return nil, fmt.Errorf("usage: gaap approvals <run-id>")
	}
	cfg.RunID = fs.Arg(0)
	cfg.DaemonAddr = envOr("GAAP_DAEMON_ADDR", cfg.DaemonAddr, "localhost:50051")
	return cfg, nil
}

// approvalsRun lists the run's pending approvals. A run with none is an empty
// list, not an error: scripts ask this question to find out.
func approvalsRun(ctx context.Context, c approvalClient, cfg *approvalsConfig, stdout io.Writer) error {
	pending, err := pendingApprovals(ctx, c, cfg.RunID)
	if err != nil {
		return err
	}

	if cfg.JSON {
		rows := make([]map[string]any, 0, len(pending))
		for _, p := range pending {
			rows = append(rows, p.row())
		}
		b, err := json.MarshalIndent(rows, "", "  ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(stdout, string(b))
		return err
	}

	if len(pending) == 0 {
		_, err := fmt.Fprintf(stdout, "no pending approvals for run %s\n", cfg.RunID)
		return err
	}
	for _, p := range pending {
		if p.RequestJSONErr != "" {
			_, _ = fmt.Fprintf(stdout, "%-30s %s\n", p.ApprovalTaskID, p.RequestJSONErr)
			continue
		}
		_, _ = fmt.Fprintf(stdout, "%-30s %-16s attempts=%-3d opens until %s\n",
			p.ApprovalTaskID, p.Request.AgentType, p.Request.Attempts, p.Request.Deadline.Format(time.RFC3339))
		_, _ = fmt.Fprintf(stdout, "  %s\n", p.Request.Reason)
	}
	return nil
}

// approveConfig is one human's answer.
type approveConfig struct {
	RunID      string
	Task       string
	Action     string
	AgentType  string
	Context    string
	Reason     string
	Agent      string
	Agents     []string
	JSON       bool
	DaemonAddr string
	APIKey     string
	TLSCert    string
}

func (c *approveConfig) settings() llmSettings {
	return llmSettings{DaemonAddr: c.DaemonAddr, APIKey: c.APIKey, TLSCert: c.TLSCert}
}

// approveCommand answers a run's pending approval.
func approveCommand(args []string, stdout io.Writer) error {
	cfg, err := parseApproveFlags(args)
	if err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()

	c, err := dialDaemon(ctx, cfg.settings())
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	return approveRun(ctx, c, cfg, stdout)
}

func parseApproveFlags(args []string) (*approveConfig, error) {
	cfg := &approveConfig{Action: "retry"}
	fs := flag.NewFlagSet("approve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	fs.StringVar(&cfg.Task, "task", "", "Task to answer, when the run has more than one pending (default: the only one)")
	fs.StringVar(&cfg.Action, "action", "retry", "What to do: retry, reassign, edit or cancel (default: retry)")
	fs.StringVar(&cfg.AgentType, "agent-type", "", "Agent type for --action reassign")
	fs.StringVar(&cfg.Context, "context", "", "JSON object of amended context for --action edit")
	fs.StringVar(&cfg.Reason, "reason", "", "Why — required, and recorded with the decision")
	fs.StringVar(&cfg.Agent, "agent", "", "Who is deciding — required, and recorded")
	agents := fs.String("agents", "", "Comma-separated allowlist of who may decide")
	fs.BoolVar(&cfg.JSON, "json", false, "Print the decision as JSON")
	fs.StringVar(&cfg.DaemonAddr, "addr", "", "Vassago daemon address (default: localhost:50051)")
	fs.StringVar(&cfg.APIKey, "api-key", "", "Vassago daemon API key (Bearer token)")
	fs.StringVar(&cfg.TLSCert, "tls-cert", "", "Path to TLS CA certificate for daemon connection")

	if err := fs.Parse(reorderArgs(args, map[string]bool{
		"task": true, "action": true, "agent-type": true, "context": true,
		"reason": true, "agent": true, "agents": true,
		"addr": true, "api-key": true, "tls-cert": true,
	})); err != nil {
		return nil, err
	}
	if fs.NArg() != 1 {
		return nil, fmt.Errorf("usage: gaap approve <run-id> [--task <task-id>] [--action ...] --reason <why> --agent <who>")
	}
	cfg.RunID = fs.Arg(0)
	cfg.Agents = splitList(*agents)
	cfg.DaemonAddr = envOr("GAAP_DAEMON_ADDR", cfg.DaemonAddr, "localhost:50051")
	return cfg, nil
}

// approveRun enacts one decision. Every refusal happens before anything is
// written: a half-answered question is worse than an unanswered one.
func approveRun(ctx context.Context, c approvalClient, cfg *approveConfig, stdout io.Writer) error {
	if cfg.Agent == "" {
		return fmt.Errorf("--agent is required: a decision with no author is not auditable")
	}
	if len(cfg.Agents) > 0 && !contains(cfg.Agents, cfg.Agent) {
		return fmt.Errorf("agent %q is not allowed to approve (allowed: %s)", cfg.Agent, strings.Join(cfg.Agents, ", "))
	}
	if cfg.AgentType != "" && gaap.ApprovalAction(cfg.Action) != gaap.ApprovalReassign {
		return fmt.Errorf("--agent-type is only valid with --action reassign")
	}

	pending, err := pendingApprovals(ctx, c, cfg.RunID)
	if err != nil {
		return err
	}
	chosen, err := selectApproval(pending, cfg.Task)
	if err != nil {
		return err
	}

	decision := gaap.ApprovalDecision{
		Action:    gaap.ApprovalAction(cfg.Action),
		AgentType: cfg.AgentType,
		Context:   cfg.Context,
		Reason:    cfg.Reason,
		DecidedBy: cfg.Agent,
		DecidedAt: time.Now().UTC(),
	}
	if err := decision.Validate(); err != nil {
		return err
	}

	body, err := json.Marshal(decision)
	if err != nil {
		return err
	}
	key := "approval_" + chosen.Request.TaskID + "_result"
	// "memory" is the daemon's blackboard target; the category is what marks this
	// entry as a decision. (vassago validates the target against an allowlist.)
	mem, err := c.AddMemory(ctx, "memory", "approval_decision", key, string(body), 5, cfg.Agent)
	if err != nil {
		return fmt.Errorf("recording the decision: %w", err)
	}
	if _, err := c.CompleteTask(ctx, chosen.ApprovalTaskID, mem.Id); err != nil {
		return fmt.Errorf("completing the approval task %s: %w", chosen.ApprovalTaskID, err)
	}

	if cfg.JSON {
		b, err := json.MarshalIndent(struct {
			ApprovalTask string                `json:"approval_task"`
			Decision     gaap.ApprovalDecision `json:"decision"`
		}{chosen.ApprovalTaskID, decision}, "", "  ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(stdout, string(b))
		return err
	}

	_, err = fmt.Fprintf(stdout, "answered %s: %s by %s\n", chosen.Request.TaskID, decision.Action, decision.DecidedBy)
	return err
}

// pendingApprovals returns the run's unanswered questions, oldest deadline
// first. A task id that is not an ApprovalRequest is reported rather than
// skipped: a request the reader cannot parse is a bug worth seeing.
func pendingApprovals(ctx context.Context, c approvalClient, runID string) ([]pendingApproval, error) {
	tasks, err := c.FindReadyTasks(ctx, gaap.ApprovalAgentType, 100)
	if err != nil {
		return nil, fmt.Errorf("listing approvals: %w", err)
	}

	var out []pendingApproval
	for _, t := range tasks {
		if runID != "" && !strings.HasPrefix(t.Id, runID+"_") {
			continue
		}
		p := pendingApproval{ApprovalTaskID: t.Id}
		if err := json.Unmarshal([]byte(t.Context), &p.Request); err != nil {
			p.RequestJSONErr = fmt.Sprintf("unreadable request: %v", err)
		}
		out = append(out, p)
	}
	return out, nil
}

// selectApproval resolves which question was meant. Guessing between two is how
// a person answers the wrong one, so an ambiguous call is an error that lists
// the candidates.
func selectApproval(pending []pendingApproval, task string) (pendingApproval, error) {
	if len(pending) == 0 {
		return pendingApproval{}, fmt.Errorf("no pending approvals")
	}

	if task == "" {
		if len(pending) > 1 {
			ids := make([]string, 0, len(pending))
			for _, p := range pending {
				ids = append(ids, p.Request.TaskID)
			}
			return pendingApproval{}, fmt.Errorf("%d pending approvals: name one with --task (%s)",
				len(pending), strings.Join(ids, ", "))
		}
		return pending[0], nil
	}

	for _, p := range pending {
		if p.ApprovalTaskID == task || p.Request.TaskID == task || strings.HasSuffix(p.Request.TaskID, "_"+task) {
			return p, nil
		}
	}
	return pendingApproval{}, fmt.Errorf("no pending approval for task %q", task)
}

// reorderArgs lets a flag and the run id appear in either order. A human types
// `gaap approve <run-id> --action retry`; Go's flag package stops at the first
// non-flag word, so without this the documented form is a usage error. valueFlags
// name the flags that take a separate value, so their argument is not mistaken
// for the run id.
func reorderArgs(args []string, valueFlags map[string]bool) []string {
	flags := make([]string, 0, len(args))
	positional := make([]string, 0, 1)
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			positional = append(positional, a)
			continue
		}
		flags = append(flags, a)
		name := strings.TrimLeft(a, "-")
		if strings.Contains(name, "=") {
			continue // --flag=value carries its own value
		}
		if valueFlags[name] && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return append(flags, positional...)
}

// splitList turns a comma-separated flag into trimmed, non-empty items.
func splitList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}
