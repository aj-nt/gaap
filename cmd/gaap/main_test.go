package main

import (
	"context"
	"net"
	"reflect"
	"strings"
	"testing"

	pb "github.com/aj-nt/vassago-sdk/proto"
	"google.golang.org/grpc"
)

// TestParseRunFlagsDefaults verifies that an empty flag set (just a goal)
// returns a config with all defaults applied.
func TestParseRunFlagsDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := parseRunFlags("run", []string{"audit the codebase"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Goal != "audit the codebase" {
		t.Errorf("expected goal 'audit the codebase', got %q", cfg.Goal)
	}
	if cfg.DaemonAddr != "localhost:50051" {
		t.Errorf("expected default daemon addr, got %q", cfg.DaemonAddr)
	}
	if cfg.MaxWaitSec != 300 {
		t.Errorf("expected default timeout 300, got %d", cfg.MaxWaitSec)
	}
	if cfg.DryRun {
		t.Error("expected DryRun=false by default")
	}
	if cfg.Model != "glm-5.1:cloud" {
		t.Errorf("expected default model 'glm-5.1:cloud', got %q", cfg.Model)
	}
	if cfg.OllamaURL != "http://localhost:11434/v1" {
		t.Errorf("expected default ollama URL, got %q", cfg.OllamaURL)
	}
	if cfg.MaxTokens != 4096 {
		t.Errorf("expected default max tokens 4096, got %d", cfg.MaxTokens)
	}
	if cfg.Temperature != 0.1 {
		t.Errorf("expected default temperature 0.1, got %v", cfg.Temperature)
	}
}

// TestParseRunFlagsExplicit verifies that all flags are parsed when set.
func TestParseRunFlagsExplicit(t *testing.T) {
	t.Parallel()

	cfg, err := parseRunFlags("run", []string{
		"--dry-run",
		"--addr", "192.168.1.1:50051",
		"--repo", "/srv/repos/gaap",
		"--timeout", "600",
		"--model", "deepseek-v4-pro:cloud",
		"--ollama-url", "http://studio:11434/v1",
		"--max-tokens", "8192",
		"--temperature", "0.3",
		"deep scan",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Goal != "deep scan" {
		t.Errorf("expected goal 'deep scan', got %q", cfg.Goal)
	}
	if cfg.DaemonAddr != "192.168.1.1:50051" {
		t.Errorf("expected explicit addr, got %q", cfg.DaemonAddr)
	}
	if cfg.RepoPath != "/srv/repos/gaap" {
		t.Errorf("expected explicit repo, got %q", cfg.RepoPath)
	}
	if cfg.MaxWaitSec != 600 {
		t.Errorf("expected explicit timeout 600, got %d", cfg.MaxWaitSec)
	}
	if !cfg.DryRun {
		t.Error("expected DryRun=true when --dry-run is set")
	}
	if cfg.Model != "deepseek-v4-pro:cloud" {
		t.Errorf("expected model, got %q", cfg.Model)
	}
	if cfg.OllamaURL != "http://studio:11434/v1" {
		t.Errorf("expected ollama URL, got %q", cfg.OllamaURL)
	}
	if cfg.MaxTokens != 8192 {
		t.Errorf("expected max-tokens 8192, got %d", cfg.MaxTokens)
	}
	if cfg.Temperature != 0.3 {
		t.Errorf("expected temperature 0.3, got %v", cfg.Temperature)
	}
}

// TestParseRunFlagsMissingGoal verifies error on missing positional goal.
func TestParseRunFlagsMissingGoal(t *testing.T) {
	t.Parallel()

	_, err := parseRunFlags("run", []string{"--dry-run"})
	if err == nil {
		t.Fatal("expected error for missing goal")
	}
	if !strings.Contains(err.Error(), "goal is required") {
		t.Errorf("expected 'goal is required' in error, got %q", err.Error())
	}
}

// TestParseRunFlagsEmptyArgs verifies error when args is nil/empty.
func TestParseRunFlagsEmptyArgs(t *testing.T) {
	t.Parallel()

	_, err := parseRunFlags("run", nil)
	if err == nil {
		t.Fatal("expected error for nil args")
	}
}

// TestParseRunFlagsInvalidTimeout verifies error on non-numeric timeout.
func TestParseRunFlagsInvalidTimeout(t *testing.T) {
	t.Parallel()

	_, err := parseRunFlags("run", []string{"--timeout", "abc", "goal"})
	if err == nil {
		t.Fatal("expected error for non-numeric timeout")
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Errorf("expected 'timeout' in error, got %q", err.Error())
	}
}

// TestParseRunFlagsInvalidMaxTokens verifies error on non-numeric max-tokens.
func TestParseRunFlagsInvalidMaxTokens(t *testing.T) {
	t.Parallel()

	_, err := parseRunFlags("run", []string{"--max-tokens", "abc", "goal"})
	if err == nil {
		t.Fatal("expected error for non-numeric max-tokens")
	}
	if !strings.Contains(err.Error(), "max-tokens") {
		t.Errorf("expected 'max-tokens' in error, got %q", err.Error())
	}
}

// TestRunConfigEquality verifies that two configs with the same fields are equal.
// This guards against future field additions breaking tests silently.
func TestRunConfigEquality(t *testing.T) {
	t.Parallel()

	a := runConfig{
		Goal:            "scan",
		DaemonAddr:      "localhost:50051",
		RepoPath:        "/tmp/repo",
		MaxWaitSec:      300,
		PollIntervalSec: 5,
		DryRun:          false,
		Model:           "glm-5.1:cloud",
		OllamaURL:       "http://localhost:11434/v1",
		MaxTokens:       4096,
		Temperature:     0.1,
	}
	b := runConfig{
		Goal:            "scan",
		DaemonAddr:      "localhost:50051",
		RepoPath:        "/tmp/repo",
		MaxWaitSec:      300,
		PollIntervalSec: 5,
		DryRun:          false,
		Model:           "glm-5.1:cloud",
		OllamaURL:       "http://localhost:11434/v1",
		MaxTokens:       4096,
		Temperature:     0.1,
	}

	if !reflect.DeepEqual(a, b) {
		t.Error("identical configs should be equal")
	}
}

// TestVersionInfo verifies that versionInfo() returns the version string
// when Version is set.
func TestVersionInfo(t *testing.T) {
	t.Parallel()

	// When Version is "dev" (default), it should return "gaap version dev".
	result := versionInfo()
	if !strings.Contains(result, "gaap version") {
		t.Errorf("versionInfo() should contain 'gaap version', got %q", result)
	}

	// Set a release version and verify it appears.
	old := Version
	Version = "v0.1.0"
	defer func() { Version = old }()

	result = versionInfo()
	if result != "gaap version v0.1.0" {
		t.Errorf("expected 'gaap version v0.1.0', got %q", result)
	}
}

// --- resume subcommand ---

// TestParseResumeFlagsValid verifies parsing of gaap resume <run-key>.
func TestParseResumeFlagsValid(t *testing.T) {
	t.Parallel()

	cfg, err := parseResumeFlags("resume", []string{"runstate_my-run"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.RunKey != "runstate_my-run" {
		t.Errorf("expected RunKey 'runstate_my-run', got %q", cfg.RunKey)
	}
	if cfg.DaemonAddr != "localhost:50051" {
		t.Errorf("expected default addr, got %q", cfg.DaemonAddr)
	}
}

// TestParseResumeFlagsWithAddr verifies --addr flag on resume.
func TestParseResumeFlagsWithAddr(t *testing.T) {
	t.Parallel()

	cfg, err := parseResumeFlags("resume", []string{"--addr", "studio:50051", "runstate_x"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.RunKey != "runstate_x" {
		t.Errorf("expected RunKey 'runstate_x', got %q", cfg.RunKey)
	}
	if cfg.DaemonAddr != "studio:50051" {
		t.Errorf("expected addr 'studio:50051', got %q", cfg.DaemonAddr)
	}
}

// TestParseResumeFlagsMissingKey verifies error when run key is missing.
func TestParseResumeFlagsMissingKey(t *testing.T) {
	t.Parallel()

	_, err := parseResumeFlags("resume", []string{})
	if err == nil {
		t.Fatal("expected error for missing run key")
	}
	if !strings.Contains(err.Error(), "run key is required") {
		t.Errorf("expected 'run key is required' in error, got %q", err.Error())
	}
}

// TestRunFlagParseError verifies that run() returns an error when flag parsing fails.
func TestRunFlagParseError(t *testing.T) {
	t.Parallel()

	err := run([]string{"--invalid-flag", "goal"})
	if err == nil {
		t.Fatal("expected error for invalid flag")
	}
	if !strings.Contains(err.Error(), "flag parsing") {
		t.Errorf("expected 'flag parsing' in error, got %q", err.Error())
	}
}

// TestRunMissingGoal verifies that run() returns an error when goal is missing.
func TestRunMissingGoal(t *testing.T) {
	t.Parallel()

	err := run([]string{})
	if err == nil {
		t.Fatal("expected error for missing goal")
	}
	if !strings.Contains(err.Error(), "goal is required") {
		t.Errorf("expected 'goal is required' in error, got %q", err.Error())
	}
}

// TestResumeFlagParseError verifies that resume() returns an error when flag parsing fails.
func TestResumeFlagParseError(t *testing.T) {
	t.Parallel()

	err := resume([]string{"--invalid-flag", "runstate_x"})
	if err == nil {
		t.Fatal("expected error for invalid flag")
	}
	if !strings.Contains(err.Error(), "flag parsing") {
		t.Errorf("expected 'flag parsing' in error, got %q", err.Error())
	}
}

// TestResumeMissingKey verifies that resume() returns an error when run key is missing.
func TestResumeMissingKey(t *testing.T) {
	t.Parallel()

	err := resume([]string{})
	if err == nil {
		t.Fatal("expected error for missing run key")
	}
	if !strings.Contains(err.Error(), "run key is required") {
		t.Errorf("expected 'run key is required' in error, got %q", err.Error())
	}
}

// TestResumeInvalidDaemonAddr verifies that resume() returns an error when daemon is unreachable.
// This covers the connection failure path in resume().
func TestResumeInvalidDaemonAddr(t *testing.T) {
	t.Parallel()

	// Use an unreachable address to force a connection error.
	err := resume([]string{"--addr", "127.0.0.1:19999", "runstate_test_key"})
	if err == nil {
		t.Fatal("expected error for unreachable daemon")
	}
	if !strings.Contains(err.Error(), "failed to connect") {
		t.Errorf("expected 'failed to connect' in error, got %q", err.Error())
	}
}

// TestParseResumeFlagsDefaults verifies that an empty flag set (just a run key)
// returns a config with all defaults applied.
func TestParseResumeFlagsDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := parseResumeFlags("resume", []string{"run-k1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.RunKey != "run-k1" {
		t.Errorf("expected RunKey 'run-k1', got %q", cfg.RunKey)
	}
	if cfg.DaemonAddr != "localhost:50051" {
		t.Errorf("expected default daemon addr, got %q", cfg.DaemonAddr)
	}
	if cfg.MaxWaitSec != 300 {
		t.Errorf("expected default timeout 300, got %d", cfg.MaxWaitSec)
	}
	if cfg.Model != "glm-5.1:cloud" {
		t.Errorf("expected default model, got %q", cfg.Model)
	}
	if cfg.MaxTokens != 4096 {
		t.Errorf("expected default max tokens 4096, got %d", cfg.MaxTokens)
	}
	if cfg.Temperature != 0.1 {
		t.Errorf("expected default temperature 0.1, got %v", cfg.Temperature)
	}
	if len(cfg.AgentTypes) != 2 || cfg.AgentTypes[0] != "static_analysis" {
		t.Errorf("expected default agent types, got %v", cfg.AgentTypes)
	}
}

// TestParseResumeFlagsExplicit verifies that all flags are parsed when set.
func TestParseResumeFlagsExplicit(t *testing.T) {
	t.Parallel()

	cfg, err := parseResumeFlags("resume", []string{
		"--addr", "192.168.1.10:50051",
		"--api-key", "sec-abc",
		"--tls-cert", "/certs/ca.pem",
		"--model", "devstral-small-2:24b-cloud",
		"--ollama-url", "http://studio:11434/v1",
		"--max-tokens", "2048",
		"--temperature", "0.5",
		"--agent-types", "static_analysis,synthesis",
		"--timeout", "600",
		"run-k2",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.RunKey != "run-k2" {
		t.Errorf("expected RunKey 'run-k2', got %q", cfg.RunKey)
	}
	if cfg.DaemonAddr != "192.168.1.10:50051" {
		t.Errorf("expected explicit addr, got %q", cfg.DaemonAddr)
	}
	if cfg.APIKey != "sec-abc" {
		t.Errorf("expected API key, got %q", cfg.APIKey)
	}
	if cfg.TLSCert != "/certs/ca.pem" {
		t.Errorf("expected TLS cert path, got %q", cfg.TLSCert)
	}
	if cfg.Model != "devstral-small-2:24b-cloud" {
		t.Errorf("expected model, got %q", cfg.Model)
	}
	if cfg.MaxTokens != 2048 {
		t.Errorf("expected max tokens 2048, got %d", cfg.MaxTokens)
	}
	if cfg.Temperature != 0.5 {
		t.Errorf("expected temperature 0.5, got %v", cfg.Temperature)
	}
	if cfg.MaxWaitSec != 600 {
		t.Errorf("expected timeout 600, got %d", cfg.MaxWaitSec)
	}
	if len(cfg.AgentTypes) != 2 || cfg.AgentTypes[0] != "static_analysis" || cfg.AgentTypes[1] != "synthesis" {
		t.Errorf("expected [static_analysis synthesis], got %v", cfg.AgentTypes)
	}
}

// TestParseResumeFlagsMissingRunKey verifies error when run key is missing.
func TestParseResumeFlagsMissingRunKey(t *testing.T) {
	t.Parallel()

	_, err := parseResumeFlags("resume", []string{"--addr", "localhost:50051"})
	if err == nil {
		t.Fatal("expected error for missing run key")
	}
	if !strings.Contains(err.Error(), "run key is required") {
		t.Errorf("expected 'run key is required' in error, got %q", err.Error())
	}
}


// smokeDaemon is a minimal gRPC server that satisfies HealthCheck by
// returning a valid GetHotBlock response.
type smokeDaemon struct {
	pb.UnimplementedVassagoServer
}

func (s *smokeDaemon) GetHotBlock(_ context.Context, _ *pb.HotBlockRequest) (*pb.HotBlockResponse, error) {
	return &pb.HotBlockResponse{Content: "ok"}, nil
}

// TestRunDryRunSmoke starts a real TCP gRPC server and exercises the full
// CLI pipeline: flag parsing, connect, health check, dry-run decomposition.
func TestRunDryRunSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping CLI smoke test in short mode")
	}

	// Start a minimal gRPC server on a random port
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	addr := lis.Addr().String()
	srv := grpc.NewServer()
	// Register a minimal server that satisfies HealthCheck (calls GetHotBlock)
	pb.RegisterVassagoServer(srv, &smokeDaemon{UnimplementedVassagoServer: pb.UnimplementedVassagoServer{}})
	go func() {
		_ = srv.Serve(lis)
	}()
	defer srv.Stop()

	// Exercise the full run() pipeline with --dry-run
	err = run([]string{
		"--addr", addr,
		"--repo", t.TempDir(),
		"--timeout", "5",
		"--dry-run",
		"smoke test",
	})
	if err != nil {
		t.Fatalf("run() failed: %v", err)
	}
}

// TestRunConnectionFailure verifies run() returns connection error on unreachable daemon.
func TestRunConnectionFailure(t *testing.T) {
	t.Parallel()

	err := run([]string{
		"--addr", "127.0.0.1:19998",
		"--timeout", "1",
		"goal",
	})
	if err == nil {
		t.Fatal("expected connection error, got nil")
	}
	if !strings.Contains(err.Error(), "failed to connect") {
		t.Errorf("expected 'failed to connect' in error, got %q", err.Error())
	}
}

// TestParseRunFlagsEnvVar verifies GAAP_* env vars provide defaults.
func TestParseRunFlagsEnvVar(t *testing.T) {
	t.Setenv("GAAP_DAEMON_ADDR", "envhost:9999")
	t.Setenv("GAAP_MODEL", "env-model")
	t.Setenv("GAAP_AGENT_TYPES", "custom,synthesis")
	t.Setenv("GAAP_API_KEY", "env-secret")

	cfg, err := parseRunFlags("run", []string{"env goal"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.DaemonAddr != "envhost:9999" {
		t.Errorf("expected env daemon addr, got %q", cfg.DaemonAddr)
	}
	if cfg.Model != "env-model" {
		t.Errorf("expected env model, got %q", cfg.Model)
	}
	if cfg.APIKey != "env-secret" {
		t.Errorf("expected env API key, got %q", cfg.APIKey)
	}
	if len(cfg.AgentTypes) != 2 || cfg.AgentTypes[0] != "custom" {
		t.Errorf("expected env agent types [custom synthesis], got %v", cfg.AgentTypes)
	}
}

// TestParseRunFlagsFlagOverridesEnv verifies CLI flags take precedence over env.
func TestParseRunFlagsFlagOverridesEnv(t *testing.T) {
	t.Setenv("GAAP_DAEMON_ADDR", "envhost:9999")
	t.Setenv("GAAP_MODEL", "env-model")

	cfg, err := parseRunFlags("run", []string{
		"--addr", "flaghost:7777",
		"--model", "flag-model",
		"goal",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.DaemonAddr != "flaghost:7777" {
		t.Errorf("expected flag daemon addr, got %q", cfg.DaemonAddr)
	}
	if cfg.Model != "flag-model" {
		t.Errorf("expected flag model, got %q", cfg.Model)
	}
}

// TestParseResumeFlagsEnvVar verifies GAAP_* env vars for resume.
func TestParseResumeFlagsEnvVar(t *testing.T) {
	t.Setenv("GAAP_DAEMON_ADDR", "envhost:9999")
	t.Setenv("GAAP_MODEL", "env-model")
	t.Setenv("GAAP_API_KEY", "env-secret")

	cfg, err := parseResumeFlags("resume", []string{"r-k1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.DaemonAddr != "envhost:9999" {
		t.Errorf("expected env daemon addr, got %q", cfg.DaemonAddr)
	}
	if cfg.Model != "env-model" {
		t.Errorf("expected env model, got %q", cfg.Model)
	}
	if cfg.APIKey != "env-secret" {
		t.Errorf("expected env API key, got %q", cfg.APIKey)
	}
}
