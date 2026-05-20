// Gaap is a model-agnostic multi-agent orchestrator on the blackboard pattern.
// It coordinates heterogeneous agents through shared memory (Vassago).
//
// Usage:
//
//	gaap run [flags] <goal>
//
// Flags:
//
//	--dry-run       Show decomposition without dispatching tasks
//	--addr string   Vassago daemon address (default: localhost:50051)
//	--repo string   Repository path to analyze (default: current directory)
//	--timeout int   Max wait for workers in seconds (default: 300)
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/aj-nt/gaap"
	"github.com/aj-nt/gaap/internal/ollama"
	"github.com/aj-nt/gaap/internal/worker"
	"github.com/aj-nt/vassago-sdk/client"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	subcmd := os.Args[1]
	switch subcmd {
	case "run":
		if err := run(os.Args[2:]); err != nil {
			slog.Error(err.Error())
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "Unknown subcommand: %s\n", subcmd)
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprint(os.Stderr, `Gaap — Model-agnostic multi-agent orchestrator

Usage:
  gaap run [flags] <goal>

Flags:
  --dry-run       Show decomposition without dispatching tasks
  --addr string   Vassago daemon address (default: localhost:50051)
  --repo string   Repository path to analyze (default: current directory)
  --timeout int   Max wait for workers in seconds (default: 300)
`)
}

// runConfig holds the parsed CLI flags for the "run" subcommand.
type runConfig struct {
	Goal            string
	DaemonAddr      string
	RepoPath        string
	MaxWaitSec      int
	PollIntervalSec int
	DryRun          bool
}

// parseRunFlags parses the flag set for "gaap run [flags] <goal>".
// It returns a runConfig with defaults applied, or an error if
// required arguments are missing or invalid.
func parseRunFlags(name string, args []string) (*runConfig, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard) // suppress flag.Parse's default stderr output

	dryRun := fs.Bool("dry-run", false, "Show decomposition without dispatching tasks")
	addr := fs.String("addr", "", "Vassago daemon address (e.g., localhost:50051)")
	repo := fs.String("repo", "", "Repository path to analyze")
	timeout := fs.Int("timeout", 0, "Max wait for workers in seconds (default: 300)")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	goal := fs.Arg(0)
	if goal == "" {
		return nil, fmt.Errorf("goal is required")
	}

	cfg := &runConfig{
		Goal:            goal,
		DaemonAddr:      *addr,
		RepoPath:        *repo,
		MaxWaitSec:      *timeout,
		PollIntervalSec: 5,
		DryRun:          *dryRun,
	}
	if cfg.DaemonAddr == "" {
		cfg.DaemonAddr = "localhost:50051"
	}
	if cfg.MaxWaitSec <= 0 {
		cfg.MaxWaitSec = 300
	}
	return cfg, nil
}

// run is the entry point for the "run" subcommand. It parses flags,
// connects to the daemon, and executes the orchestration pipeline.
//
// Errors are returned instead of calling os.Exit — the caller (main)
// decides how to handle them. This makes run testable.
func run(args []string) error {
	rc, err := parseRunFlags("run", args)
	if err != nil {
		return fmt.Errorf("flag parsing: %w", err)
	}

	// LLM config — hardcoded defaults; flags come later
	const (
		defaultOllamaURL = "http://localhost:11434/v1"
		defaultModel     = "glm-5.1:cloud"
		defaultMaxTokens = 4096
		defaultTemp      = 0.1
	)

	if rc.RepoPath == "" {
		wd, _ := os.Getwd()
		rc.RepoPath = wd
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle SIGINT/SIGTERM
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		slog.Info("Received shutdown signal")
		cancel()
	}()

	slog.Info("Gaap starting",
		"goal", rc.Goal,
		"daemon", rc.DaemonAddr,
		"repo", rc.RepoPath,
		"dry_run", rc.DryRun,
		"model", defaultModel,
	)

	// Connect to Vassago daemon
	daemonClient, err := client.Connect(ctx, client.Config{Address: rc.DaemonAddr})
	if err != nil {
		return fmt.Errorf("failed to connect to Vassago daemon at %s: %w", rc.DaemonAddr, err)
	}
	defer daemonClient.Close()

	if err := daemonClient.HealthCheck(ctx); err != nil {
		return fmt.Errorf("Vassago daemon health check failed: %w", err)
	}

	// Build LLM decomposer
	ollamaClient := ollama.NewClient(ollama.Config{
		BaseURL:     defaultOllamaURL,
		Model:       defaultModel,
		MaxTokens:   defaultMaxTokens,
		Temperature: defaultTemp,
		TimeoutSec:  120,
	})

	// Bridge ollama.Client.Chat ([]Message) → chatFn (ctx, prompt string)
	chatFn := func(ctx context.Context, prompt string) (string, error) {
		return ollamaClient.Chat([]ollama.Message{{Role: "user", Content: prompt}})
	}

	decomposer := gaap.NewDecomposer(gaap.NewLLMDecomposition(chatFn))

	orchestratorCfg := &gaap.Config{
		DaemonAddr:      rc.DaemonAddr,
		RepoPath:        rc.RepoPath,
		MaxWaitSec:      rc.MaxWaitSec,
		PollIntervalSec: rc.PollIntervalSec,
	}
	orchestrator := gaap.NewOrchestrator(ctx, orchestratorCfg, daemonClient, decomposer)
	orchestrator.SetSynthesisChatFn(chatFn) // LLM synthesis with schema fallback

	// Auto-workers: spawn worker pool to execute tasks in-process.
	if !rc.DryRun {
		wpCfg := worker.PoolConfig{
			DaemonAddr:  rc.DaemonAddr,
			AgentID:     "gaap-worker",
			AgentName:   "gaap-worker",
			AgentTypes:  []string{"static_analysis", "quality_scan"},
			WorkerCount: 2,
			PollSec:     2,
			MaxTurns:    20,
			RepoPath:    rc.RepoPath,
			Ollama: ollama.Config{
				BaseURL:     defaultOllamaURL,
				Model:       defaultModel,
				MaxTokens:   defaultMaxTokens,
				Temperature: defaultTemp,
				TimeoutSec:  120,
			},
		}
		pool, err := worker.NewPool(ctx, wpCfg, daemonClient)
		if err != nil {
			slog.Warn("Failed to create worker pool, running without auto-workers", "error", err)
		} else {
			orchestrator.SetWorkerPool(pool)
			slog.Info("Auto-workers enabled", "agent_types", wpCfg.AgentTypes, "count", wpCfg.WorkerCount)
		}
	}

	if rc.DryRun {
		tasks, err := decomposer.Decompose(ctx, rc.Goal, rc.RepoPath)
		if err != nil {
			return fmt.Errorf("decomposition failed: %w", err)
		}
		fmt.Printf("\nDecomposition for: %s\n\n", rc.Goal)
		for _, t := range tasks {
			fmt.Printf("  %-35s [%-20s] %s\n", t.TaskID, t.AgentType, t.Status)
			if len(t.ParentIDs) > 0 {
				fmt.Printf("    depends on: %v\n", t.ParentIDs)
			}
		}
		fmt.Println()
		return nil
	}

	if err := orchestrator.Run(rc.Goal); err != nil {
		return fmt.Errorf("orchestration failed: %w", err)
	}

	slog.Info("Gaap complete", "goal", rc.Goal)
	return nil
}
