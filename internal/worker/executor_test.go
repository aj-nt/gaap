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
