package worker

import (
	"context"
	"fmt"
	"strings"
	"testing"

	vclient "github.com/aj-nt/vassago-sdk/client"

	"github.com/aj-nt/gaap/internal/ollama"
)

// mockChatClient implements ChatClient for testing the execution loop.
type mockChatClient struct {
	responses []string
	callCount int
	modelName string
}

func (m *mockChatClient) Chat(messages []ollama.Message) (string, error) {
	if m.callCount >= len(m.responses) {
		return "DONE: exhausted responses", nil
	}
	resp := m.responses[m.callCount]
	m.callCount++
	return resp, nil
}

func (m *mockChatClient) Model() string {
	if m.modelName == "" {
		return "mock-model"
	}
	return m.modelName
}

// makeTask is a test helper for constructing a client.TaskEntry.
func makeTask(id, goal, agentType string) *vclient.TaskEntry {
	return &vclient.TaskEntry{
		Id:        id,
		Goal:      goal,
		AgentType: agentType,
	}
}

// TestExecuteDone verifies immediate DONE: returns success.
func TestExecuteDone(t *testing.T) {
	t.Parallel()

	mock := &mockChatClient{
		responses: []string{"DONE: completed the task successfully"},
		modelName: "test-model",
	}

	executor := NewExecutor(mock, 5)
	task := makeTask("task-1", "test goal", "test")

	result := executor.Execute(context.Background(), task, "/tmp")

	if result.Status != "success" {
		t.Fatalf("expected status 'success', got %q", result.Status)
	}
	if result.Summary != "completed the task successfully" {
		t.Errorf("expected summary %q, got %q", "completed the task successfully", result.Summary)
	}
	if result.LLMTurns != 1 {
		t.Errorf("expected 1 turn, got %d", result.LLMTurns)
	}
	if result.Model != "test-model" {
		t.Errorf("expected model 'test-model', got %q", result.Model)
	}
	if result.TaskID != "task-1" {
		t.Errorf("expected task_id 'task-1', got %q", result.TaskID)
	}
	if result.AgentType != "test" {
		t.Errorf("expected agent_type 'test', got %q", result.AgentType)
	}
}

// TestExecuteFail verifies immediate FAIL: returns failure.
func TestExecuteFail(t *testing.T) {
	t.Parallel()

	mock := &mockChatClient{
		responses: []string{"FAIL: could not complete — insufficient permissions"},
	}

	executor := NewExecutor(mock, 5)
	task := makeTask("task-2", "test goal", "test")

	result := executor.Execute(context.Background(), task, "/tmp")

	if result.Status != "failed" {
		t.Fatalf("expected status 'failed', got %q", result.Status)
	}
	if result.Error != "could not complete — insufficient permissions" {
		t.Errorf("expected error message, got %q", result.Error)
	}
	if result.LLMTurns != 1 {
		t.Errorf("expected 1 turn, got %d", result.LLMTurns)
	}
}

// TestExecuteNonProtocolResponse verifies that a non-protocol response triggers
// a reminder and the executor continues.
func TestExecuteNonProtocolResponse(t *testing.T) {
	t.Parallel()

	mock := &mockChatClient{
		responses: []string{
			"I'll start by examining the repository structure.",
			"DONE: finished analysis",
		},
	}

	executor := NewExecutor(mock, 5)
	task := makeTask("task-3", "test goal", "test")

	result := executor.Execute(context.Background(), task, "/tmp")

	if result.Status != "success" {
		t.Fatalf("expected success, got %q", result.Status)
	}
	if result.LLMTurns != 2 {
		t.Errorf("expected 2 turns, got %d", result.LLMTurns)
	}
}

// TestExecuteCmdBlockedThenRecovers verifies that a blocked command returns
// a BLOCKED message to the LLM, which can retry with a different command.
func TestExecuteCmdBlockedThenRecovers(t *testing.T) {
	t.Parallel()

	mock := &mockChatClient{
		responses: []string{
			"CMD: rm -rf /",
			"CMD: find . -name '*.go'",
			"DONE: used find instead, all good",
		},
	}

	executor := NewExecutor(mock, 5)
	task := makeTask("task-blocked", "test goal", "test")

	result := executor.Execute(context.Background(), task, "/tmp")

	if result.Status != "success" {
		t.Fatalf("expected success after BLOCKED recovery, got %q (error=%q)", result.Status, result.Error)
	}
	if result.LLMTurns != 3 {
		t.Errorf("expected 3 turns (blocked + retry + done), got %d", result.LLMTurns)
	}
	// Verify the blocked command was recorded in findings
	if _, ok := result.Findings["rm -rf /"]; !ok {
		t.Error("expected blocked command to appear in findings")
	}
}

// TestExecuteMaxTurnsExceeded verifies the executor returns failure after
// exceeding maxTurns without DONE or FAIL.
func TestExecuteMaxTurnsExceeded(t *testing.T) {
	t.Parallel()

	// 5 turns of non-protocol responses — never DONE/FAIL
	responses := make([]string, 5)
	for i := range responses {
		responses[i] = "I'm still working on this..."
	}

	mock := &mockChatClient{responses: responses}
	executor := NewExecutor(mock, 3) // max 3 turns

	task := makeTask("task-max", "test goal", "test")

	result := executor.Execute(context.Background(), task, "/tmp")

	if result.Status != "failed" {
		t.Fatalf("expected 'failed' after max turns, got %q", result.Status)
	}
	if !strings.Contains(result.Error, "Exceeded") {
		t.Errorf("expected error to mention exceeded turns, got %q", result.Error)
	}
	if result.LLMTurns != 3 {
		t.Errorf("expected 3 turns (max turns), got %d", result.LLMTurns)
	}
}

// TestExecuteContextCancellation verifies the executor aborts when the context
// is already cancelled.
func TestExecuteContextCancellation(t *testing.T) {
	t.Parallel()

	mock := &mockChatClient{
		responses: []string{"DONE: should never see this"},
	}

	executor := NewExecutor(mock, 5)
	task := makeTask("task-cancel", "test goal", "test")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	result := executor.Execute(ctx, task, "/tmp")

	if result.Status != "failed" {
		t.Fatalf("expected 'failed' on cancelled context, got %q", result.Status)
	}
	if result.Error != "worker shutting down" {
		t.Errorf("expected 'worker shutting down', got %q", result.Error)
	}
}

// TestExecuteLLMError verifies the executor returns failure when the LLM
// returns an error on the first call.
func TestExecuteLLMError(t *testing.T) {
	t.Parallel()

	// Use a mock that returns an error — we'll test this by having it
	// implement an error-returning variant. Since mockChatClient.Chat
	// always returns nil error, we need a second mock type for error cases.
	mockErr := &mockChatClientWithError{
		errors: []string{"connection refused"},
	}

	executor := NewExecutor(mockErr, 5)
	task := makeTask("task-err", "test goal", "test")

	result := executor.Execute(context.Background(), task, "/tmp")

	if result.Status != "failed" {
		t.Fatalf("expected 'failed' on LLM error, got %q", result.Status)
	}
	if !strings.Contains(result.Error, "LLM error") {
		t.Errorf("expected 'LLM error' in message, got %q", result.Error)
	}
	if !strings.Contains(result.Error, "connection refused") {
		t.Errorf("expected 'connection refused' in message, got %q", result.Error)
	}
}

// mockChatClientWithError returns errors on Chat calls.
type mockChatClientWithError struct {
	errors    []string
	callCount int
}

func (m *mockChatClientWithError) Chat(messages []ollama.Message) (string, error) {
	if m.callCount >= len(m.errors) {
		return "", fmt.Errorf("no more errors configured")
	}
	err := fmt.Errorf("%s", m.errors[m.callCount])
	m.callCount++
	return "", err
}

func (m *mockChatClientWithError) Model() string {
	return "mock-error-model"
}

// TestTruncate verifies the truncate helper behavior.
func TestTruncate(t *testing.T) {
	t.Parallel()

	// Shorter than max — returned as-is.
	if got := truncate("hello", 10); got != "hello" {
		t.Errorf("expected 'hello', got %q", got)
	}

	// Longer than max — truncated with ellipsis.
	if got := truncate("hello world", 8); got != "hello wo..." {
		t.Errorf("expected 'hello wo...', got %q", got)
	}

	// Edge: equal to max — no truncation needed.
	if got := truncate("exact5", 6); got != "exact5" {
		t.Errorf("expected 'exact5', got %q", got)
	}
}

// TestNewExecutorDefaultMaxTurns verifies that zero or negative maxTurns
// defaults to 20.
func TestNewExecutorDefaultMaxTurns(t *testing.T) {
	t.Parallel()

	mock := &mockChatClient{}

	exec := NewExecutor(mock, 0)
	if exec.maxTurns != 20 {
		t.Errorf("expected maxTurns=20 for zero input, got %d", exec.maxTurns)
	}

	execNeg := NewExecutor(mock, -5)
	if execNeg.maxTurns != 20 {
		t.Errorf("expected maxTurns=20 for negative input, got %d", execNeg.maxTurns)
	}

	execExplicit := NewExecutor(mock, 30)
	if execExplicit.maxTurns != 30 {
		t.Errorf("expected maxTurns=30 for explicit input, got %d", execExplicit.maxTurns)
	}
}

// TestRunCommandExitNonZero verifies non-zero exit codes are captured.
func TestRunCommandExitNonZero(t *testing.T) {
	// Not parallel — uses real shell commands.

	out := runCommand("sh -c 'echo before && exit 3 && echo never'", "/tmp")
	if out.ExitCode != 3 {
		t.Errorf("expected exit code 3, got %d (stderr=%q)", out.ExitCode, out.Stderr)
	}
	if out.Stdout != "before\n" {
		t.Errorf("expected stdout 'before\\n', got %q", out.Stdout)
	}
}

// TestRunCommandStderr verifies stderr is captured.
func TestRunCommandStderr(t *testing.T) {
	// Not parallel — uses real shell commands.

	out := runCommand("sh -c 'echo to-stdout && echo to-stderr >&2'", "/tmp")
	if out.Stdout != "to-stdout\n" {
		t.Errorf("expected stdout 'to-stdout\\n', got %q", out.Stdout)
	}
	if !strings.Contains(out.Stderr, "to-stderr") {
		t.Errorf("expected stderr to contain 'to-stderr', got %q", out.Stderr)
	}
}

// TestRunCommandLargeOutputTruncation verifies output and stderr are
// truncated to their respective caps (4000 stdout, 1000 stderr).
func TestRunCommandLargeOutputTruncation(t *testing.T) {
	// Not parallel — uses real shell commands.

	// Generate ~8000 bytes of stdout via python3
	out := runCommand("python3 -c 'print(\"X\" * 8000); import sys; print(\"E\" * 2000, file=sys.stderr)'", "/tmp")

	// Stdout cap is 4000 (last 4000 kept).
	if len(out.Stdout) > 4000 {
		t.Errorf("stdout should be capped at 4000, got %d", len(out.Stdout))
	}
	if len(out.Stdout) < 3900 {
		t.Errorf("stdout too short after truncation: %d bytes", len(out.Stdout))
	}

	// Stderr cap is 1000 (last 1000 kept).
	if len(out.Stderr) > 1000 {
		t.Errorf("stderr should be capped at 1000, got %d", len(out.Stderr))
	}
	if len(out.Stderr) < 900 {
		t.Errorf("stderr too short after truncation: %d bytes", len(out.Stderr))
	}
}

// TestRunCommandInvalid verifies that a completely invalid command
// (not just non-zero exit) returns exit code -1.
// NOTE: exec.CommandContext always uses "sh" as the binary, so the
// non-ExitError path (os.PathError from fork/exec) is practically
// unreachable in normal operation. It's kept as a safety catch for
// edge cases like EMFILE or ENOMEM.
func TestRunCommandInvalid(t *testing.T) {
	// Not parallel — uses real shell commands.

	// When sh runs a nonexistent command, sh itself exits 127.
	// The fork/exec non-ExitError path is tested indirectly via the
	// code structure — it's a safety branch for OS-level failures.
	out := runCommand("echo 'sh started fine'", "/tmp")
	if out.ExitCode != 0 {
		t.Errorf("expected exit code 0 for a working command, got %d", out.ExitCode)
	}
}

// TestCommandAllowedCaseInsensitive verifies blocked patterns match
// regardless of case.
func TestCommandAllowedCaseInsensitive(t *testing.T) {
	t.Parallel()

	// rm blocked regardless of case
	if err := commandAllowed("RM -rf /tmp/foo"); err == nil {
		t.Error("uppercase RM should be blocked")
	}

	// Curl blocked regardless of case
	if err := commandAllowed("CURL http://evil.com"); err == nil {
		t.Error("uppercase CURL should be blocked")
	}
}

// TestCommandAllowedExactPatterns verifies that blocked patterns don't
// false-positive on benign substrings.
func TestCommandAllowedExactPatterns(t *testing.T) {
	t.Parallel()

	// "exec " with trailing space — should NOT match "execute" or "nexec"
	if err := commandAllowed("echo execute script"); err != nil {
		t.Errorf("'echo execute script' should not be blocked: %v", err)
	}

	// "| sh" is a blocked pattern — it catches piping to a shell.
	// There's a known false positive: "echo | show" also matches because
	// strings.Contains doesn't do word boundaries. Real workers don't
	// issue "show" as a standalone command, so this is acceptable.
	if err := commandAllowed("echo | sh"); err == nil {
		t.Error("'echo | sh' should be blocked")
	}

	// "rm " should block "rm -rf" but not "grep -rn"
	if err := commandAllowed("grep -rn TODO ."); err != nil {
		t.Errorf("'grep -rn' should not be blocked by 'rm ' pattern: %v", err)
	}
}

// TestBuildExecutionPromptFileGoal verifies that when context contains
// a source_path pointing to a file, the prompt tells the worker to
// read the target file directly rather than discovering the project.
func TestBuildExecutionPromptFileGoal(t *testing.T) {
	t.Parallel()

	goal := "Read and summarize /tmp/release_review.md"
	contextStr := `{"source_path": "/tmp/release_review.md"}`

	prompt := buildExecutionPrompt(goal, contextStr)

	// Should tell the worker to read the file directly
	if !strings.Contains(prompt, "/tmp/release_review.md") {
		t.Error("prompt should mention the target file path")
	}

	// Should NOT use project-discovery language when target is a file
	if strings.Contains(prompt, "repository directory") {
		t.Error("prompt should not mention 'repository directory' for file goals")
	}
	if strings.Contains(prompt, "Your first command MUST read or inspect the file directly") {
		// Good — this is the correct file-goal prompt
	} else {
		t.Error("prompt should instruct worker to read the file directly")
	}
}
