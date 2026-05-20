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
//	--subscribe     Enable push-based task updates via gRPC (falls back to polling)
//	--agent-types string  Comma-separated agent types (default: static_analysis,quality_scan)
//	--api-key string      Vassago daemon API key (Bearer token)
//	--tls-cert string     Path to TLS CA certificate for daemon connection
//	--model string        LLM model name (default: glm-5.1:cloud)
//	--ollama-url string   Ollama base URL (default: http://localhost:11434/v1)
//	--max-tokens int      Max tokens for LLM responses (default: 4096)
//	--temperature float   LLM temperature, 0.0-1.0 (default: 0.1)
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
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

// Version is set at build time by -ldflags "-X main.Version=...".
// Defaults to "dev" for local builds.
var Version = "dev"

// versionInfo returns the gaap version string.
func versionInfo() string {
	return "gaap version " + Version
}

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	subcmd := os.Args[1]
	switch subcmd {
	case "version":
		fmt.Println(versionInfo())
	case "run":
		if err := run(os.Args[2:]); err != nil {
			slog.Error(err.Error())
			os.Exit(1)
		}
	case "resume":
		if err := resume(os.Args[2:]); err != nil {
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
  gaap resume [--addr <daemon>] [--api-key <key>] [--tls-cert <path>] <run-key>
  gaap version

Run flags:
  --dry-run         Show decomposition without dispatching tasks
  --addr string     Vassago daemon address (default: localhost:50051)
  --repo string     Repository path to analyze (default: current directory)
  --timeout int     Max wait for workers in seconds (default: 300)
  --subscribe       Enable push-based task updates via gRPC (falls back to polling)
  --agent-types string  Comma-separated agent types (default: static_analysis,quality_scan)
  --api-key string      Vassago daemon API key (Bearer token)
  --tls-cert string     Path to TLS CA certificate for daemon connection
  --model string        LLM model name (default: glm-5.1:cloud)
  --ollama-url string   Ollama base URL (default: http://localhost:11434/v1)
  --max-tokens int      Max tokens for LLM responses (default: 4096)
  --temperature float   LLM temperature, 0.0-1.0 (default: 0.1)
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
	Subscribe       bool
	Model           string
	OllamaURL       string
	MaxTokens       int
	Temperature     float64
	AgentTypes      []string
	APIKey          string
	TLSCert         string
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
	model := fs.String("model", "", "LLM model name (default: glm-5.1:cloud)")
	ollamaURL := fs.String("ollama-url", "", "Ollama base URL (default: http://localhost:11434/v1)")
	maxTokens := fs.Int("max-tokens", 0, "Max tokens for LLM responses (default: 4096)")
	temperature := fs.Float64("temperature", 0, "LLM temperature, 0.0-1.0 (default: 0.1)")
	subscribe := fs.Bool("subscribe", false, "Use gRPC subscription for push-based task updates (falls back to polling)")
	agentTypes := fs.String("agent-types", "", "Comma-separated agent types to dispatch (default: static_analysis,quality_scan)")
	apiKey := fs.String("api-key", "", "Vassago daemon API key (Bearer token)")
	tlsCert := fs.String("tls-cert", "", "Path to TLS CA certificate for daemon connection")

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
		Subscribe:       *subscribe,
		Model:           *model,
		OllamaURL:       *ollamaURL,
		MaxTokens:       *maxTokens,
		Temperature:     *temperature,
		APIKey:          *apiKey,
		TLSCert:         *tlsCert,
	}
	if cfg.DaemonAddr == "" {
		cfg.DaemonAddr = "localhost:50051"
	}
	if cfg.MaxWaitSec <= 0 {
		cfg.MaxWaitSec = 300
	}
	if cfg.Model == "" {
		cfg.Model = "glm-5.1:cloud"
	}
	if cfg.OllamaURL == "" {
		cfg.OllamaURL = "http://localhost:11434/v1"
	}
	if cfg.MaxTokens <= 0 {
		cfg.MaxTokens = 4096
	}
	if cfg.Temperature <= 0 {
		cfg.Temperature = 0.1
	}
	if *agentTypes != "" {
		cfg.AgentTypes = strings.Split(*agentTypes, ",")
	} else {
		cfg.AgentTypes = []string{"static_analysis", "quality_scan"}
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
		"model", rc.Model,
	)

	// Connect to Vassago daemon
	daemonClient, err := client.Connect(ctx, client.Config{
		Address: rc.DaemonAddr,
		APIKey:  rc.APIKey,
		TLSCert: rc.TLSCert,
	})
	if err != nil {
		return fmt.Errorf("failed to connect to Vassago daemon at %s: %w", rc.DaemonAddr, err)
	}
	defer daemonClient.Close()

	if err := daemonClient.HealthCheck(ctx); err != nil {
		return fmt.Errorf("Vassago daemon health check failed: %w", err)
	}

	// Build LLM decomposer
	ollamaClient := ollama.NewClient(ollama.Config{
		BaseURL:     rc.OllamaURL,
		Model:       rc.Model,
		MaxTokens:   rc.MaxTokens,
		Temperature: rc.Temperature,
		TimeoutSec:  120,
	})

	// Bridge ollama.Client.Chat ([]Message) → chatFn (ctx, prompt string)
	chatFn := func(ctx context.Context, prompt string) (string, error) {
		return ollamaClient.Chat([]ollama.Message{{Role: "user", Content: prompt}})
	}

	// Build agent catalog from CLI agent types
	catalog := make(map[string]gaap.AgentSpec)
	for _, at := range rc.AgentTypes {
		if spec, ok := gaap.DefaultAgentCatalog[at]; ok {
			catalog[at] = spec
		}
	}
	// Always include synthesis regardless of CLI types
	if _, ok := catalog["synthesis"]; !ok {
		if spec, ok := gaap.DefaultAgentCatalog["synthesis"]; ok {
			catalog["synthesis"] = spec
		}
	}

	decomposer := gaap.NewDecomposer(gaap.NewLLMDecomposition(chatFn, catalog))

	orchestratorCfg := &gaap.Config{
		DaemonAddr:      rc.DaemonAddr,
		RepoPath:        rc.RepoPath,
		MaxWaitSec:      rc.MaxWaitSec,
		PollIntervalSec: rc.PollIntervalSec,
	}
	orchestrator := gaap.NewOrchestrator(ctx, orchestratorCfg, daemonClient, decomposer)
	orchestrator.SetSynthesisChatFn(chatFn) // LLM synthesis with schema fallback

	// Enable push-based task updates via gRPC subscription (with polling fallback).
	if rc.Subscribe {
		orchestrator.SetSubscribeFallbackToPoll(true)
		slog.Info("Observer mode: gRPC subscription enabled with polling fallback")
	}

	// Auto-workers: spawn worker pool to execute tasks in-process.
	if !rc.DryRun {
		wpCfg := worker.PoolConfig{
			DaemonAddr:  rc.DaemonAddr,
			AgentID:     "gaap-worker",
			AgentName:   "gaap-worker",
			AgentTypes:  rc.AgentTypes,
			WorkerCount: 2,
			PollSec:     2,
			MaxTurns:    20,
			RepoPath:    rc.RepoPath,
			Ollama: ollama.Config{
				BaseURL:     rc.OllamaURL,
				Model:       rc.Model,
				MaxTokens:   rc.MaxTokens,
				Temperature: rc.Temperature,
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

// resumeConfig holds parsed flags for "gaap resume".
type resumeConfig struct {
	RunKey     string
	DaemonAddr string
	APIKey     string
	TLSCert    string
}

// parseResumeFlags parses "gaap resume [--addr <daemon>] [--api-key <key>] [--tls-cert <path>] <run-key>".
func parseResumeFlags(name string, args []string) (*resumeConfig, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	addr := fs.String("addr", "", "Vassago daemon address (default: localhost:50051)")
	apiKey := fs.String("api-key", "", "Vassago daemon API key (Bearer token)")
	tlsCert := fs.String("tls-cert", "", "Path to TLS CA certificate for daemon connection")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	runKey := fs.Arg(0)
	if runKey == "" {
		return nil, fmt.Errorf("run key is required")
	}

	cfg := &resumeConfig{
		RunKey:     runKey,
		DaemonAddr: *addr,
		APIKey:     *apiKey,
		TLSCert:    *tlsCert,
	}
	if cfg.DaemonAddr == "" {
		cfg.DaemonAddr = "localhost:50051"
	}
	return cfg, nil
}

// resume loads a saved orchestration run from the daemon and resumes it
// from the Waiting state.
func resume(args []string) error {
	rc, err := parseResumeFlags("resume", args)
	if err != nil {
		return fmt.Errorf("flag parsing: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		slog.Info("Received shutdown signal")
		cancel()
	}()

	slog.Info("Gaap resuming", "run_key", rc.RunKey, "daemon", rc.DaemonAddr)

	daemonClient, err := client.Connect(ctx, client.Config{
		Address: rc.DaemonAddr,
		APIKey:  rc.APIKey,
		TLSCert: rc.TLSCert,
	})
	if err != nil {
		return fmt.Errorf("failed to connect to Vassago daemon at %s: %w", rc.DaemonAddr, err)
	}
	defer daemonClient.Close()

	if err := daemonClient.HealthCheck(ctx); err != nil {
		return fmt.Errorf("Vassago daemon health check failed: %w", err)
	}

	rs, err := gaap.LoadRunState(ctx, daemonClient, rc.RunKey)
	if err != nil {
		return fmt.Errorf("load run state: %w", err)
	}

	orchestratorCfg := &gaap.Config{
		DaemonAddr:      rc.DaemonAddr,
		RepoPath:        rs.RepoPath,
		MaxWaitSec:      300,
		PollIntervalSec: 5,
	}
	orchestrator := gaap.NewOrchestrator(ctx, orchestratorCfg, daemonClient, nil)

	if err := orchestrator.Resume(rs); err != nil {
		return fmt.Errorf("resume: %w", err)
	}

	slog.Info("Gaap resume complete", "run_key", rc.RunKey)
	return nil
}
