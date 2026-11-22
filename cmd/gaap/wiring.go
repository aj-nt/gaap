// This file is part of Gaap.
// See LICENSE-Apache-2.0 for license information.

package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/aj-nt/gaap"
	"github.com/aj-nt/gaap/internal/ollama"
	"github.com/aj-nt/gaap/internal/worker"
	"github.com/aj-nt/gaap/platform"
	"github.com/aj-nt/vassago-sdk/client"
)

// llmSettings is the subset of CLI flags both subcommands need to reach the
// daemon and the model. run and resume each project their own config onto it,
// so the wiring below exists once instead of twice.
type llmSettings struct {
	DaemonAddr  string
	APIKey      string
	TLSCert     string
	OllamaURL   string
	Model       string
	MaxTokens   int
	Temperature float64
	AgentTypes  []string
}

// connFields are the flags every daemon-touching subcommand carries. run and
// resume embed them, so the projection onto llmSettings exists once: a field
// added to one command's flags and not the other is a resume that talks to the
// wrong daemon and reports it as an empty task list.
type connFields struct {
	DaemonAddr  string
	APIKey      string
	TLSCert     string
	OllamaURL   string
	Model       string
	MaxTokens   int
	Temperature float64
	AgentTypes  []string
}

// settings is a conversion, not a field-by-field copy: the two structs must keep
// the same fields in the same order, so a flag added to the CLI and not carried
// to the wiring stops compiling instead of silently arriving empty.
func (c connFields) settings() llmSettings { return llmSettings(c) }

// governFields carries the flags that decide how a run is governed, and governFlagSet is
// their parsed form — the seam registerGovernFlags hands back so the flag text, the
// environment variable and the default are stated once.
//
// These seven were registered and projected separately in `run` and `resume` before this,
// which is the defect connFields exists to prevent: a governance flag added to one command
// and forgotten in the other is a resumed run that dispatches around the policy it was
// started with. Both commands embed this.
type governFields struct {
	// BreakerThreshold/BreakerCooldownSec enable per-agent-type circuit breakers for the
	// run; zero threshold leaves them off. A resumed run has to be told again, because
	// breakers are not part of the persisted run state.
	BreakerThreshold   int
	BreakerCooldownSec int
	// Policy governs the run: every dispatch is decided against it and recorded. Empty
	// means ungoverned, which is the default — a run is not governed by accident.
	Policy     string
	StateDir   string
	TrustFloor float64
	// RateLimit turns a `rate_limit` rule into a deny carrying a cooldown. Off means the
	// rule still denies, without promising a retry window.
	RateLimit bool
	// AllowHostExec accepts uncoerced execution on the host. Off by default, and named
	// after what it permits rather than what it disables.
	AllowHostExec bool
}

type governFlagSet struct {
	breakerThreshold *int
	breakerCooldown  *int
	policy           *string
	stateDir         *string
	trustFloor       *float64
	rateLimit        *bool
	allowHostExec    *bool
}

// registerGovernFlags declares the governance flags on fs and returns their parsed form.
// Flags are declared here rather than in each parser because the usage text is part of the
// interface: two copies of it drift, and the copy a user reads is whichever command they
// happened to type.
func registerGovernFlags(fs *flag.FlagSet) governFlagSet {
	return governFlagSet{
		breakerThreshold: fs.Int("breaker-threshold", 0, "Open an agent type's circuit breaker after N dead-lettered tasks (default: 0 = off)"),
		breakerCooldown:  fs.Int("breaker-cooldown", 0, "Seconds an open breaker refuses dispatch (default: 60 when breakers are on)"),
		policy:           fs.String("policy", "", "Policy file: govern every dispatch against it"),
		stateDir:         fs.String("state", defaultStateDir, "State directory for the ledger, keys and kill switch"),
		trustFloor:       fs.Float64("trust-floor", 0, "Escalate allows to review while trust is below this (0 disables)"),
		rateLimit:        fs.Bool("rate-limit", false, "Make a rate_limit rule deny with a cooldown and a count"),
		allowHostExec:    fs.Bool("allow-host-exec", false, "Run worker commands on the host, uncoerced (default: refuse unless containment is available)"),
	}
}

// settings resolves the environment fallbacks. Flag, then environment, then default.
func (f governFlagSet) settings() governFields {
	return governFields{
		BreakerThreshold:   atoiOr(envOr("GAAP_BREAKER_THRESHOLD", "", ""), *f.breakerThreshold, 0),
		BreakerCooldownSec: atoiOr(envOr("GAAP_BREAKER_COOLDOWN", "", ""), *f.breakerCooldown, 0),
		Policy:             envOr("GAAP_POLICY", *f.policy, ""),
		StateDir:           envOr("GAAP_STATE", *f.stateDir, defaultStateDir),
		TrustFloor:         *f.trustFloor,
		RateLimit:          *f.rateLimit,
		AllowHostExec:      *f.allowHostExec,
	}
}

// signalContext returns a context cancelled by SIGINT/SIGTERM.
func signalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		slog.Info("Received shutdown signal")
		cancel()
	}()
	return ctx, cancel
}

// dialDaemon dials the Vassago daemon and confirms it answers. A daemon that
// accepts the connection but fails its health check is not usable, so the
// connection is closed rather than handed back.
func dialDaemon(ctx context.Context, s llmSettings) (client.MnemoClient, error) {
	c, err := client.Connect(ctx, client.Config{
		Address: s.DaemonAddr,
		APIKey:  s.APIKey,
		TLSCert: s.TLSCert,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to connect to Vassago daemon at %s: %w", s.DaemonAddr, err)
	}
	if err := c.HealthCheck(ctx); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("vassago daemon health check failed: %w", err)
	}
	return c, nil
}

// ollamaConfig projects the CLI settings onto the client's config. Two callers
// build a client from the same flags — the chat bridge and the worker pool — and
// a field added to one of them and not the other is a run whose decomposer and
// workers disagree about the model.
func ollamaConfig(s llmSettings) ollama.Config {
	return ollama.Config{
		BaseURL:     s.OllamaURL,
		Model:       s.Model,
		MaxTokens:   s.MaxTokens,
		Temperature: s.Temperature,
		TimeoutSec:  120,
	}
}

// chatFunc builds the LLM client and the (ctx, prompt) → string bridge both
// the decomposer and the synthesis engine consume.
func chatFunc(s llmSettings) func(context.Context, string) (string, error) {
	c := ollama.NewClient(ollamaConfig(s))
	return func(_ context.Context, prompt string) (string, error) {
		return c.Chat([]ollama.Message{{Role: "user", Content: prompt}})
	}
}

// workerAgentTypes drops the approval agent type from a pool's claim set. A
// pool that claimed approvals would answer the orchestrator's questions itself —
// the one thing an approval exists to prevent.
func workerAgentTypes(types []string) []string {
	out := make([]string, 0, len(types))
	for _, t := range types {
		if strings.TrimSpace(t) == gaap.ApprovalAgentType {
			slog.Warn("ignoring the approval agent type: approvals are for humans, not workers",
				"agent_type", t)
			continue
		}
		out = append(out, t)
	}
	return out
}

// commandRunner picks the execution path for worker tasks, and the order of these
// branches is the security decision.
//
// Fail-closed is the whole point of W3b: this worker called `sh -c` unconditionally
// for the life of the product, so the default has to be "refuse", and running on the
// host has to be something an operator asked for by name. vagent's equivalent fails
// open — it logs and leaves the in-process runner installed under a config comment
// claiming deny-by-default — and copying that shape here would have produced a seam
// that governs nothing while making the code look governed.
func commandRunner(ctx context.Context, allowHostExec bool, repoPath string) worker.CommandRunner {
	if allowHostExec {
		slog.Warn("worker: executing on the HOST, uncoerced (allow_host_exec is set)", "workspace", repoPath)
		return worker.HostRunner{}
	}
	return containedRunner(ctx, repoPath, &platform.DockerEnforcer{})
}

// containedRunner is commandRunner with the enforcer supplied, so the fail-closed
// branch is testable without a docker daemon.
func containedRunner(ctx context.Context, repoPath string, enf platform.Enforcer) worker.CommandRunner {
	uid := execUID()
	image := os.Getenv("GAAP_EXEC_IMAGE")
	if image == "" {
		image = "alpine:latest"
	}
	workspace := os.Getenv("GAAP_EXEC_WORKSPACE")
	if workspace == "" {
		workspace = repoPath
	}
	if err := enf.Ensure(ctx); err != nil {
		// Not a warning that we then work around: coercion is unavailable, so the
		// commands do not run. The reason travels into the task's failure text so the
		// operator sees why a run produced no output.
		slog.Error("worker: containment unavailable — Execute is refused for this run",
			"err", err, "hint", "install docker on a Linux host, or pass --allow-host-exec to accept uncoerced execution")
		return worker.RefuseRunner{Why: "containment unavailable: " + err.Error()}
	}
	runner := worker.NewContainerRunner(enf, uid, workspace, image)
	slog.Info("worker: command execution containerized", "runner", runner.Describe())
	return runner
}

// execUID is the per-agent uid, from the environment so a deploy can pin it. A
// shared uid means two agents share a filesystem view, which is a blast radius
// rather than a boundary.
func execUID() int {
	if v := os.Getenv("GAAP_EXEC_UID"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
		slog.Warn("worker: GAAP_EXEC_UID is not a positive integer; using the default", "value", v)
	}
	return 10001
}

// startWorkers spawns the in-process worker pool and attaches it to the
// orchestrator. A pool that cannot start is a warning, not a failure: the run
// continues without auto-workers and tasks stay queued.
func startWorkers(ctx context.Context, s llmSettings, repoPath string, daemon client.MnemoClient, o *gaap.Orchestrator, allowHostExec bool) {
	wpCfg := worker.PoolConfig{
		DaemonAddr:  s.DaemonAddr,
		AgentID:     "gaap-worker",
		AgentName:   "gaap-worker",
		AgentTypes:  workerAgentTypes(s.AgentTypes),
		WorkerCount: 2,
		PollSec:     2,
		MaxTurns:    20,
		RepoPath:    repoPath,
		Ollama:      ollamaConfig(s),
		Runner:      commandRunner(ctx, allowHostExec, repoPath),
	}
	pool, err := worker.NewPool(ctx, wpCfg, daemon)
	if err != nil {
		slog.Warn("Failed to create worker pool, running without auto-workers", "error", err)
		return
	}
	o.SetWorkerPool(pool)
	slog.Info("Auto-workers enabled", "agent_types", wpCfg.AgentTypes, "count", wpCfg.WorkerCount)
}
