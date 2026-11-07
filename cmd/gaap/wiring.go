// This file is part of Gaap.
// See LICENSE-Apache-2.0 for license information.

package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/aj-nt/gaap"
	"github.com/aj-nt/gaap/internal/ollama"
	"github.com/aj-nt/gaap/internal/worker"
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

func (c *runConfig) settings() llmSettings {
	return llmSettings{
		DaemonAddr:  c.DaemonAddr,
		APIKey:      c.APIKey,
		TLSCert:     c.TLSCert,
		OllamaURL:   c.OllamaURL,
		Model:       c.Model,
		MaxTokens:   c.MaxTokens,
		Temperature: c.Temperature,
		AgentTypes:  c.AgentTypes,
	}
}

func (c *resumeConfig) settings() llmSettings {
	return llmSettings{
		DaemonAddr:  c.DaemonAddr,
		APIKey:      c.APIKey,
		TLSCert:     c.TLSCert,
		OllamaURL:   c.OllamaURL,
		Model:       c.Model,
		MaxTokens:   c.MaxTokens,
		Temperature: c.Temperature,
		AgentTypes:  c.AgentTypes,
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

// chatFunc builds the LLM client and the (ctx, prompt) → string bridge both
// the decomposer and the synthesis engine consume.
func chatFunc(s llmSettings) func(context.Context, string) (string, error) {
	c := ollama.NewClient(ollama.Config{
		BaseURL:     s.OllamaURL,
		Model:       s.Model,
		MaxTokens:   s.MaxTokens,
		Temperature: s.Temperature,
		TimeoutSec:  120,
	})
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

// startWorkers spawns the in-process worker pool and attaches it to the
// orchestrator. A pool that cannot start is a warning, not a failure: the run
// continues without auto-workers and tasks stay queued.
func startWorkers(ctx context.Context, s llmSettings, repoPath string, daemon client.MnemoClient, o *gaap.Orchestrator) {
	wpCfg := worker.PoolConfig{
		DaemonAddr:  s.DaemonAddr,
		AgentID:     "gaap-worker",
		AgentName:   "gaap-worker",
		AgentTypes:  workerAgentTypes(s.AgentTypes),
		WorkerCount: 2,
		PollSec:     2,
		MaxTurns:    20,
		RepoPath:    repoPath,
		Ollama: ollama.Config{
			BaseURL:     s.OllamaURL,
			Model:       s.Model,
			MaxTokens:   s.MaxTokens,
			Temperature: s.Temperature,
			TimeoutSec:  120,
		},
	}
	pool, err := worker.NewPool(ctx, wpCfg, daemon)
	if err != nil {
		slog.Warn("Failed to create worker pool, running without auto-workers", "error", err)
		return
	}
	o.SetWorkerPool(pool)
	slog.Info("Auto-workers enabled", "agent_types", wpCfg.AgentTypes, "count", wpCfg.WorkerCount)
}
