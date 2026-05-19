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
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/aj-nt/gaap"
	"github.com/aj-nt/gaap/internal/ollama"
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
		runArgs()
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

func runArgs() {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	dryRun := fs.Bool("dry-run", false, "Show decomposition without dispatching tasks")
	addr := fs.String("addr", "", "Vassago daemon address (e.g., localhost:50051)")
	repo := fs.String("repo", "", "Repository path to analyze")
	timeout := fs.Int("timeout", 0, "Max wait for workers in seconds (default: 300)")

	fs.Parse(os.Args[2:])

	goal := fs.Arg(0)
	if goal == "" {
		slog.Error("Usage: gaap run <goal>")
		os.Exit(1)
	}

	// LLM config — hardcoded defaults; flags come later
	const (
		defaultOllamaURL = "http://localhost:11434/v1"
		defaultModel     = "glm-5.1:cloud"
		defaultMaxTokens = 2000
		defaultTemp      = 0.1
	)

	cfg := &gaap.Config{
		DaemonAddr:      *addr,
		RepoPath:        *repo,
		MaxWaitSec:      *timeout,
		PollIntervalSec: 5,
	}
	if cfg.DaemonAddr == "" {
		cfg.DaemonAddr = "localhost:50051"
	}
	if cfg.RepoPath == "" {
		wd, _ := os.Getwd()
		cfg.RepoPath = wd
	}
	if cfg.MaxWaitSec <= 0 {
		cfg.MaxWaitSec = 300
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
		"goal", goal,
		"daemon", cfg.DaemonAddr,
		"repo", cfg.RepoPath,
		"dry_run", *dryRun,
		"model", defaultModel,
	)

	// Connect to Vassago daemon
	daemonClient, err := client.Connect(ctx, client.Config{Address: cfg.DaemonAddr})
	if err != nil {
		slog.Error("Failed to connect to Vassago daemon", "addr", cfg.DaemonAddr, "error", err)
		os.Exit(1)
	}
	defer daemonClient.Close()

	if err := daemonClient.HealthCheck(ctx); err != nil {
		slog.Error("Vassago daemon health check failed", "error", err)
		os.Exit(1)
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

	orchestrator := gaap.NewOrchestrator(ctx, cfg, daemonClient, decomposer)

	if *dryRun {
		tasks, err := decomposer.Decompose(ctx, goal, cfg.RepoPath)
		if err != nil {
			slog.Error("Decomposition failed", "error", err)
			os.Exit(1)
		}
		fmt.Printf("\nDecomposition for: %s\n\n", goal)
		for _, t := range tasks {
			fmt.Printf("  %-35s [%-20s] %s\n", t.TaskID, t.AgentType, t.Status)
			if len(t.ParentIDs) > 0 {
				fmt.Printf("    depends on: %v\n", t.ParentIDs)
			}
		}
		fmt.Println()
		return
	}

	if err := orchestrator.Run(goal); err != nil {
		slog.Error("Orchestrator failed", "error", err)
		os.Exit(1)
	}

	slog.Info("Gaap complete", "goal", goal)
}
