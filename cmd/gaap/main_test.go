package main

import (
	"reflect"
	"strings"
	"testing"
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
