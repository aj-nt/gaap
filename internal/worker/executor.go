package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"github.com/aj-nt/gaap/internal/ollama"
	"github.com/aj-nt/vassago-sdk/client"
)

// allowedCommands is the set of commands workers are permitted to run.
// Only the first word of a CMD: line is checked against this list.
// Commands with pipes/redirects still use sh -c, but the first word gates access.
var allowedCommands = map[string]bool{
	// File discovery and inspection
	"find":     true,
	"ls":       true,
	"grep":     true,
	"cat":      true,
	"head":     true,
	"tail":     true,
	"wc":       true,
	"sort":     true,
	"du":       true,
	"file":     true,
	"stat":     true,
	"diff":     true,

	// Shell utilities
	"cd":       true,
	"pwd":      true,
	"echo":     true,
	"which":    true,
	"type":     true,
	"command":  true,

	// Go toolchain (read-only operations)
	"go":       true,

	// Static analysis tools
	"golangci-lint": true,
	"gosec":         true,
	"govulncheck":   true,

	// Text processing (read-only, no -i)
	"awk":      true,
	"sed":      true,

	// Scripting (limited)
	"python3":  true,
	"node":     true,
	"sh":       true, // needed for pipelines; guarded by pattern checks

	// Source control (read-only)
	"git":      true,
}

// blockedPatterns are substrings that, if present anywhere in the command,
// cause it to be rejected. These catch destructive operations regardless
// of which tool name is used.
var blockedPatterns = []string{
	"rm ", "rm\t", "rmdir", "chmod", "chown", "chgrp",
	"kill", "killall", "pkill", "shutdown", "reboot", "halt",
	"sudo ", "su ", "doas ",
	"> /dev/", "dd if=", "mkfs", "mount ", "umount",
	"curl ", "wget ", "nc ", "ncat ",
	"git push", "git commit", "git tag", "git branch -D", "git branch -d",
	"openssl", "ssh ", "scp ", "sftp ",
	"~/.ssh", "~/.aws", "/etc/passwd", "/etc/shadow",
	"| sh", "| bash", "| zsh",
	".bashrc", ".zshrc", ".profile",
	"eval ", "exec ",
}

// ChatClient is the interface the executor uses to communicate with an LLM.
// This abstraction enables testing the execution loop with mock implementations.
type ChatClient interface {
	Chat(messages []ollama.Message) (string, error)
	Model() string
}

// commandAllowed checks whether a shell command string is safe to execute.
// Returns nil if allowed, or an error explaining why it was blocked.
func commandAllowed(shellCmd string) error {
	cmd := strings.TrimSpace(shellCmd)

	// Empty command passes nothing to exec but isn't useful — reject cleanly
	if cmd == "" {
		return fmt.Errorf("command blocked: empty")
	}

	// Check blocked patterns first — these are unconditional rejections
	cmdLower := strings.ToLower(cmd)
	for _, pattern := range blockedPatterns {
		if strings.Contains(cmdLower, pattern) {
			return fmt.Errorf("command blocked: contains %q", pattern)
		}
	}

	// Check that the first word is in our allowlist
	firstWord := strings.Fields(cmd)[0]
	if !allowedCommands[firstWord] {
		return fmt.Errorf("command %q is not in the allowlist", firstWord)
	}

	return nil
}

// Executor runs a single task via the CMD:/DONE:/FAIL: protocol.
type Executor struct {
	llm      ChatClient
	maxTurns int
}

// NewExecutor creates a task executor with the given LLM config.
func NewExecutor(llm ChatClient, maxTurns int) *Executor {
	if maxTurns <= 0 {
		maxTurns = 20
	}
	return &Executor{llm: llm, maxTurns: maxTurns}
}

// ExecuteResult is the result of executing a task.
type ExecuteResult struct {
	TaskID      string         `json:"task_id"`
	AgentType   string         `json:"agent_type,omitempty"`
	Status      string         `json:"status"`
	Summary     string         `json:"summary,omitempty"`
	Error       string         `json:"error,omitempty"`
	Findings    map[string]any `json:"findings"`
	Model       string         `json:"model,omitempty"`
	LLMTurns    int            `json:"llm_turns"`
	DurationMs  int64          `json:"duration_ms"`
	CompletedAt int64          `json:"completed_at,omitempty"`
}

// Execute runs the CMD:/DONE:/FAIL: protocol against a task from the daemon.
func (e *Executor) Execute(ctx context.Context, task *client.TaskEntry, repoPath string) *ExecuteResult {
	t0 := time.Now()

	// Parse task context for repo_path
	contextStr := task.Context
	if contextStr == "" {
		contextStr = "{}"
	}
	if repoPath == "" {
		var taskCtx map[string]any
		if json.Unmarshal([]byte(contextStr), &taskCtx) == nil {
			if rp, ok := taskCtx["source_path"].(string); ok && rp != "" {
				repoPath = rp
			}
		}
	}

	messages := []ollama.Message{{
		Role:    "user",
		Content: buildExecutionPrompt(task.Goal, contextStr),
	}}

	findings := make(map[string]any)

	for turn := 1; turn <= e.maxTurns; turn++ {
		select {
		case <-ctx.Done():
			return &ExecuteResult{TaskID: task.Id, Status: "failed", Error: "worker shutting down"}
		default:
		}

		text, err := e.llm.Chat(messages)
		if err != nil {
			return &ExecuteResult{
				TaskID:     task.Id,
				Status:     "failed",
				Error:      fmt.Sprintf("LLM error: %v", err),
				Findings:   findings,
				DurationMs: time.Since(t0).Milliseconds(),
			}
		}

		text = strings.TrimSpace(text)
		messages = append(messages, ollama.Message{Role: "assistant", Content: text})

		switch {
		case strings.HasPrefix(text, "CMD:"):
			cmd := strings.TrimSpace(text[4:])
			slog.Info("worker: running command", "turn", turn, "cmd", truncate(cmd, 80))

			// Security check — reject dangerous commands before execution
			if allowErr := commandAllowed(cmd); allowErr != nil {
				slog.Warn("worker: command blocked", "turn", turn, "cmd", truncate(cmd, 80), "reason", allowErr)
				response := fmt.Sprintf("BLOCKED: %s\n\nAvailable tools: find, ls, grep, cat, head, tail, wc, sort, go, golangci-lint, gosec, govulncheck, git (read-only), awk, sed (read-only), python3, node, echo, which, file, diff, du, stat, cd, pwd", allowErr)
				messages = append(messages, ollama.Message{Role: "user", Content: response})
				findings[truncate(cmd, 60)] = map[string]any{
					"exit_code":     -1,
					"output_length": 0,
					"blocked":       true,
				}
				continue
			}

			output := runCommand(cmd, repoPath)
			findings[truncate(cmd, 60)] = map[string]any{
				"exit_code":     output.ExitCode,
				"output_length": output.Len(),
			}

			response := fmt.Sprintf("Command output:\n%s", output.Stdout)
			if output.Stderr != "" {
				response += fmt.Sprintf("\n[stderr]: %s", truncate(output.Stderr, 1000))
			}
			if output.ExitCode != 0 {
				response += fmt.Sprintf("\n[exit: %d]", output.ExitCode)
			}
			messages = append(messages, ollama.Message{Role: "user", Content: response})

		case strings.HasPrefix(text, "DONE:"):
			summary := strings.TrimSpace(text[5:])
			slog.Info("worker: task DONE", "id", task.Id, "summary", truncate(summary, 100), "turns", turn)
			return &ExecuteResult{
				TaskID:      task.Id,
				AgentType:   task.AgentType,
				Status:      "success",
				Summary:     summary,
				Findings:    findings,
				Model:       e.llm.Model(),
				LLMTurns:    turn,
				DurationMs:  time.Since(t0).Milliseconds(),
				CompletedAt: time.Now().Unix(),
			}

		case strings.HasPrefix(text, "FAIL:"):
			reason := strings.TrimSpace(text[5:])
			slog.Warn("worker: task FAIL", "id", task.Id, "reason", truncate(reason, 100), "turns", turn)
			return &ExecuteResult{
				TaskID:     task.Id,
				Status:     "failed",
				Error:      reason,
				Findings:   findings,
			Model:      e.llm.Model(),
			LLMTurns:   turn,
			DurationMs: time.Since(t0).Milliseconds(),
		}

	default:
		messages = append(messages, ollama.Message{
			Role:    "user",
			Content: "Respond with CMD:, DONE:, or FAIL:",
		})
	}
}

return &ExecuteResult{
	TaskID:     task.Id,
	Status:     "failed",
	Error:      fmt.Sprintf("Exceeded %d turns without DONE/FAIL", e.maxTurns),
	Findings:   findings,
	Model:      e.llm.Model(),
	LLMTurns:   e.maxTurns,
	DurationMs: time.Since(t0).Milliseconds(),
}
}

// cmdOutput holds the result of running a shell command.
type cmdOutput struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Len returns the total output length.
func (c cmdOutput) Len() int { return len(c.Stdout) + len(c.Stderr) }

// runCommand executes a shell command with a 60-second timeout.
func runCommand(shellCmd, repoPath string) cmdOutput {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	c := exec.CommandContext(ctx, "sh", "-c", shellCmd)
	if repoPath != "" {
		c.Dir = repoPath
	}

	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr

	err := c.Run()
	exitCode := 0
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return cmdOutput{Stdout: "", Stderr: "[TIMEOUT after 60s]", ExitCode: -1}
		}
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
		}
	}

	out := stdout.String()
	if len(out) > 4000 {
		out = out[len(out)-4000:]
	}
	errOut := stderr.String()
	if len(errOut) > 1000 {
		errOut = errOut[len(errOut)-1000:]
	}

	return cmdOutput{Stdout: out, Stderr: errOut, ExitCode: exitCode}
}

// buildExecutionPrompt constructs the system+user prompt for task execution.
func buildExecutionPrompt(goal, contextStr string) string {
	return fmt.Sprintf(`You are an autonomous worker agent executing a task.

Your job: accomplish the goal using shell commands. You have terminal access
to read-only analysis tools within the repository directory.

PROTOCOL — respond with exactly one of these on each turn:
CMD: <shell command>
DONE: <summary of what you accomplished>
FAIL: <reason>

RULES:
- One command per CMD: line. Keep commands focused.
- Use head/tail to limit large output.
- Never run destructive commands. Only analysis and inspection tools.
- Commands timeout after 60 seconds.
- You have a budget of ~20 turns. After turn 15, start wrapping up.
- When you have enough data to answer, DONE: immediately.
- Analyze output before issuing the next command.
- If a command is blocked, try a different approach — don't retry the same one.

AVAILABLE TOOLS: find, ls, grep, cat, head, tail, wc, sort, du, file, stat,
diff, cd, pwd, echo, which, go (build/test/vet/fmt/list), golangci-lint,
gosec, govulncheck, awk, sed, python3, node, git (status/log/diff/show).

CONTEXT:
%s

GOAL:
%s

Start now: output your first CMD: line.`, contextStr, goal)
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
