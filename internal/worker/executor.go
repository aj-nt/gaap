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
	"github.com/aj-nt/gaap/internal/status"
	"github.com/aj-nt/vassago-sdk/client"
)

// allowedCommands is the set of commands workers are permitted to run.
// Every simple command in a CMD: line (split on unquoted ; && || | and
// newlines) is checked: its program word must appear here.
//
// This is a blast-radius limiter, not a security boundary: python3, node and
// sh are on the list, so a worker that wants to do something is never more
// than one line away from doing it. Its job is to keep the honest model on
// read-only tools and to make the intent of a command legible, not to contain
// a hostile one.
var allowedCommands = map[string]bool{
	// File discovery and inspection
	"find": true,
	"ls":   true,
	"grep": true,
	"cat":  true,
	"head": true,
	"tail": true,
	"wc":   true,
	"sort": true,
	"du":   true,
	"file": true,
	"stat": true,
	"diff": true,

	// Shell utilities
	"cd":      true,
	"pwd":     true,
	"echo":    true,
	"which":   true,
	"type":    true,
	"command": true,

	// Go toolchain (read-only operations)
	"go": true,

	// Static analysis tools
	"golangci-lint": true,
	"gosec":         true,
	"govulncheck":   true,

	// Text processing (read-only, no -i)
	"awk": true,
	"sed": true,

	// Scripting (limited)
	"python3": true,
	"node":    true,
	"sh":      true, // needed for pipelines; guarded per-segment below

	// Source control (read-only subcommands only)
	"git": true,
}

// shellInterpreters are allowed as the first word of a command but not as the
// target of a pipe — `echo x | sh` executes arbitrary text, which is the whole
// reason the pipeline form is refused.
var shellInterpreters = map[string]bool{"sh": true, "bash": true, "zsh": true}

// findExecSafe is the set of programs a `find -exec` may invoke. `find
// -exec <prog>` is the only form of find that reports per-file facts (sizes,
// line counts), so it has to be allowed; what actually matters is <prog>.
var findExecSafe = map[string]bool{
	"wc": true, "grep": true, "cat": true, "head": true, "tail": true,
	"du": true, "file": true, "stat": true, "echo": true, "printf": true,
	"sort": true, "md5sum": true, "shasum": true, "cmp": true, "diff": true,
	"basename": true, "dirname": true, "tee": false, // tee writes, listed to stay explicit
}

// gitReadOnly is the set of git subcommands that cannot move a ref, write the
// index, or touch the network.
var gitReadOnly = map[string]bool{
	"status": true, "log": true, "diff": true, "show": true,
	"rev-parse": true, "ls-files": true, "grep": true, "blame": true,
	"describe": true, "cat-file": true, "shortlog": true,
	"for-each-ref": true, "name-rev": true,
	"branch": true, "tag": true, "remote": true, // read-only forms only, see below
}

// gitWriteFlags are the flags that turn an otherwise read-only subcommand into
// a write, so `git branch -D x` and `git tag -a v1` stay blocked.
var gitWriteFlags = map[string]map[string]bool{
	"branch": {
		"-d": true, "-D": true, "-m": true, "-M": true, "-f": true,
		"--delete": true, "--move": true, "--force": true, "--set-upstream-to": true,
	},
	"tag": {
		"-a": true, "-d": true, "-f": true, "-m": true, "-s": true,
		"--annotate": true, "--delete": true, "--force": true, "--sign": true,
	},
}

// gitRemoteReadOnly are the sub-forms of `git remote` that only read.
var gitRemoteReadOnly = map[string]bool{
	"-v": true, "--verbose": true, "show": true, "get-url": true,
}

// sensitiveTokens are substrings that must not appear in a command argument.
// They are matched against tokens, never against the whole command string, so
// a grep pattern or an echo string that merely names a path is not a violation.
var sensitiveTokens = []string{
	"~/.ssh", ".ssh/id_", "~/.aws", "/etc/passwd", "/etc/shadow",
	".bashrc", ".zshrc", ".profile",
}

// ChatClient is the interface the executor uses to communicate with an LLM.
// This abstraction enables testing the execution loop with mock implementations.
type ChatClient interface {
	Chat(messages []ollama.Message) (string, error)
	Model() string
}

// commandAllowed checks whether a shell command string is safe to execute.
// Returns nil if allowed, or an error explaining why it was blocked.
//
// The command is split into simple commands and each one is checked on its own
// program word and arguments. The earlier implementation scanned the whole
// string for substrings, which had both failure modes at once: it blocked
// `find -exec wc -l` (the correct command for "file sizes") because the
// pattern list contained "exec ", and grepping a repository for its kill
// switch because the list contained "kill" — which a model can walk around by
// writing 'k'+'i'+'l'+'l', as one did.
func commandAllowed(shellCmd string) error {
	checked := 0
	for i, segment := range splitSimpleCommands(shellCmd) {
		tokens := shellTokens(segment)
		for len(tokens) > 0 && isEnvAssignment(tokens[0]) {
			tokens = tokens[1:]
		}
		if len(tokens) == 0 {
			continue
		}
		checked++

		prog := tokens[0]
		if !allowedCommands[prog] {
			return fmt.Errorf("command %q is not in the allowlist", prog)
		}
		if i > 0 && shellInterpreters[prog] {
			return fmt.Errorf("command blocked: piping into %q runs arbitrary text", prog)
		}
		if err := checkArguments(prog, tokens); err != nil {
			return err
		}
	}

	if checked == 0 {
		return fmt.Errorf("command blocked: empty")
	}
	return nil
}

// checkArguments applies the per-program rules that a first-word allowlist
// cannot express, plus the checks that apply to every program.
func checkArguments(prog string, tokens []string) error {
	switch prog {
	case "find":
		for j := 1; j < len(tokens); j++ {
			switch tokens[j] {
			case "-exec", "-execdir", "-ok", "-okdir":
				if j+1 >= len(tokens) {
					return fmt.Errorf("command blocked: find %s with no program after it", tokens[j])
				}
				target := tokens[j+1]
				if !findExecSafe[target] {
					return fmt.Errorf("command blocked: find %s %s", tokens[j], target)
				}
			}
		}

	case "sed":
		for _, t := range tokens[1:] {
			if t == "-i" || strings.HasPrefix(t, "-i.") || strings.HasPrefix(t, "--in-place") {
				return fmt.Errorf("command blocked: sed edits in place (%q)", t)
			}
		}

	case "git":
		if len(tokens) < 2 {
			return nil // bare `git` prints usage
		}
		sub := tokens[1]
		if !gitReadOnly[sub] {
			return fmt.Errorf("command blocked: git %q is not a read-only subcommand", sub)
		}
		for _, t := range tokens[2:] {
			if gitWriteFlags[sub][t] {
				return fmt.Errorf("command blocked: git %s %q writes", sub, t)
			}
		}
		if sub == "remote" && len(tokens) > 2 && !gitRemoteReadOnly[tokens[2]] {
			return fmt.Errorf("command blocked: git remote %q is not read-only", tokens[2])
		}
	}

	for _, t := range tokens[1:] {
		if err := checkDeviceWrite(t); err != nil {
			return err
		}
		for _, s := range sensitiveTokens {
			if strings.Contains(t, s) {
				return fmt.Errorf("command blocked: %q references %q", t, s)
			}
		}
	}
	return nil
}

// checkDeviceWrite blocks redirects at block devices while leaving /dev/null
// alone. The old check banned the substring "> /dev/", which also banned
// `2>/dev/null` — a thing every shell script does.
func checkDeviceWrite(token string) error {
	const prefix = "/dev/"
	idx := strings.Index(token, prefix)
	if idx < 0 {
		return nil
	}
	rest := token[idx+len(prefix):]
	if k := strings.IndexAny(rest, ";|&\"'<>"); k >= 0 {
		rest = rest[:k]
	}
	switch {
	case rest == "null", rest == "stdout", rest == "stderr", rest == "stdin":
		return nil
	case strings.HasPrefix(rest, "fd/"):
		return nil
	}
	return fmt.Errorf("command blocked: writes to device %q", token)
}

// splitSimpleCommands splits a command line into simple commands on unquoted,
// unescaped ; && || | & and newlines. Heredoc bodies are skipped entirely: they
// are data, and treating a line of python as a shell command is how a filter
// ends up lying about what it checked.
func splitSimpleCommands(cmd string) []string {
	var out []string
	var cur strings.Builder
	runes := []rune(cmd)

	flush := func() {
		out = append(out, cur.String())
		cur.Reset()
	}

	var quote rune
	heredocDelim := ""
	inHeredoc := false
	heredocPending := false

	i := 0
	for i < len(runes) {
		r := runes[i]

		if inHeredoc {
			j := i
			for j < len(runes) && runes[j] != '\n' {
				j++
			}
			if strings.TrimSpace(string(runes[i:j])) == heredocDelim {
				inHeredoc = false
				heredocDelim = ""
			}
			i = j + 1
			continue
		}

		if quote != 0 {
			cur.WriteRune(r)
			if r == quote {
				quote = 0
			}
			i++
			continue
		}

		switch r {
		case '\'', '"':
			quote = r
			cur.WriteRune(r)
			i++
		case '\\':
			cur.WriteRune(r)
			if i+1 < len(runes) {
				cur.WriteRune(runes[i+1])
				i += 2
			} else {
				i++
			}
		case '\n':
			flush()
			if heredocPending {
				heredocPending = false
				inHeredoc = heredocDelim != ""
			}
			i++
		case ';', '&', '|':
			// `2>&1` is an fd duplication, not a background separator.
			if r == '&' && i+1 < len(runes) && runes[i+1] >= '0' && runes[i+1] <= '9' {
				cur.WriteRune(r)
				i++
				continue
			}
			flush()
			if i+1 < len(runes) && runes[i+1] == r {
				i++ // && or ||
			}
			i++
		case '<':
			if i+1 < len(runes) && runes[i+1] == '<' {
				i += 2
				if i < len(runes) && runes[i] == '-' {
					i++
				}
				for i < len(runes) && (runes[i] == ' ' || runes[i] == '	') {
					i++
				}
				var delim strings.Builder
				var dq rune
				for i < len(runes) {
					c := runes[i]
					if dq != 0 {
						if c == dq {
							dq = 0
							i++
							continue
						}
						delim.WriteRune(c)
						i++
						continue
					}
					if c == '\'' || c == '"' {
						dq = c
						i++
						continue
					}
					if c == ' ' || c == '	' || c == '\n' || c == ';' || c == '|' || c == '&' {
						break
					}
					delim.WriteRune(c)
					i++
				}
				heredocDelim = delim.String()
				heredocPending = true
				cur.WriteString("<<")
				continue
			}
			cur.WriteRune(r)
			i++
		default:
			cur.WriteRune(r)
			i++
		}
	}
	flush()
	return out
}

// shellTokens splits one simple command into words, honouring single and double
// quotes and backslash escapes, and stripping the quotes. No expansion is
// performed — a token is the literal text the shell would receive.
func shellTokens(s string) []string {
	var toks []string
	var cur strings.Builder

	flush := func() {
		if cur.Len() > 0 {
			toks = append(toks, cur.String())
			cur.Reset()
		}
	}

	runes := []rune(s)
	var quote rune
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if quote != 0 {
			if r == quote {
				quote = 0
				continue
			}
			cur.WriteRune(r)
			continue
		}
		switch r {
		case '\'', '"':
			quote = r
		case '\\':
			if i+1 < len(runes) {
				i++
				cur.WriteRune(runes[i])
			}
		case ' ', '	', '\n', '\r':
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return toks
}

// isEnvAssignment reports whether a token is a leading NAME=value prefix.
func isEnvAssignment(tok string) bool {
	eq := strings.Index(tok, "=")
	if eq <= 0 {
		return false
	}
	for i, r := range tok[:eq] {
		switch {
		case r == '_':
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
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
	Status      status.Result  `json:"status"`
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
	// attempt is 1-based: RetryCount is how many times the task has already
	// been claimed and failed, so the first execution is attempt 1.
	attempt := int(task.RetryCount) + 1

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
		Content: buildExecutionPrompt(task.Goal, contextStr, repoPath, e.maxTurns),
	}}

	findings := make(map[string]any)

	for turn := 1; turn <= e.maxTurns; turn++ {
		select {
		case <-ctx.Done():
			return &ExecuteResult{TaskID: task.Id, Status: status.ResultFailed, Error: "worker shutting down"}
		default:
		}

		text, err := e.llm.Chat(messages)
		if err != nil {
			return &ExecuteResult{
				TaskID:     task.Id,
				Status:     status.ResultFailed,
				Error:      fmt.Sprintf("LLM error: %v", err),
				Findings:   findings,
				DurationMs: time.Since(t0).Milliseconds(),
			}
		}

		text = strings.TrimSpace(text)
		messages = append(messages, ollama.Message{Role: "assistant", Content: text})

		kind, body, parsed := parseReply(text)
		if replyHadMarkup(text) {
			slog.Info("worker: stripped tool markup from reply", "id", task.Id, "attempt", attempt, "turn", turn)
		}

		switch {
		case !parsed:
			// Burning a turn on an unparseable response is invisible unless it
			// is logged: the model said something, nobody recorded what.
			slog.Warn("worker: unparseable response", "id", task.Id, "attempt", attempt, "turn", turn, "max_turns", e.maxTurns, "text", truncate(text, 80))
			messages = append(messages, ollama.Message{
				Role:    "user",
				Content: "Respond with CMD:, DONE:, or FAIL:",
			})

		case kind == actionCmd:
			cmd := body
			slog.Info("worker: running command", "id", task.Id, "attempt", attempt, "turn", turn, "cmd", truncate(cmd, 80))

			// Security check — reject dangerous commands before execution
			if allowErr := commandAllowed(cmd); allowErr != nil {
				slog.Warn("worker: command blocked", "id", task.Id, "attempt", attempt, "turn", turn, "cmd", truncate(cmd, 80), "reason", allowErr)
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

		case kind == actionDone:
			summary := body
			slog.Info("worker: task DONE", "id", task.Id, "attempt", attempt, "summary", truncate(summary, 100), "turns", turn)
			return &ExecuteResult{
				TaskID:      task.Id,
				AgentType:   task.AgentType,
				Status:      status.ResultSuccess,
				Summary:     summary,
				Findings:    findings,
				Model:       e.llm.Model(),
				LLMTurns:    turn,
				DurationMs:  time.Since(t0).Milliseconds(),
				CompletedAt: time.Now().Unix(),
			}

		case kind == actionFail:
			reason := body
			slog.Warn("worker: task FAIL", "id", task.Id, "attempt", attempt, "reason", truncate(reason, 100), "turns", turn)
			return &ExecuteResult{
				TaskID:     task.Id,
				Status:     status.ResultFailed,
				Error:      reason,
				Findings:   findings,
				Model:      e.llm.Model(),
				LLMTurns:   turn,
				DurationMs: time.Since(t0).Milliseconds(),
			}
		}
	}

	slog.Warn("worker: turn budget exhausted", "id", task.Id, "attempt", attempt, "max_turns", e.maxTurns)
	return &ExecuteResult{
		TaskID:     task.Id,
		Status:     status.ResultFailed,
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

// workingDirectoryBlock tells the model where its commands run.
//
// Without it a model assumes a path of its own invention — one run answered
// "/workspace is empty, with no Go files or go.mod" while the executor was
// running commands inside the repository. An empty finding set and a clean
// project look identical downstream, so the assumption has to be removed at the
// source rather than detected afterwards.
func workingDirectoryBlock(workdir string) string {
	if workdir == "" {
		return `WORKING DIRECTORY: unknown to the harness — your commands run in the worker's
current directory. Run pwd as your first command and work from what it prints.
Do not assume a path such as /workspace.`
	}
	return fmt.Sprintf(`WORKING DIRECTORY: %s
Every command runs with that directory as its working directory. Use relative
paths ("." / "./internal/..."), not absolute paths you have invented. If you need
to confirm, run pwd. Do not assume a path such as /workspace.`, workdir)
}

// turnBudget returns the budget to state in the prompt and the turn after
// which to ask the model to start concluding. A quarter of the budget is
// reserved for writing up what was gathered, so a 20-turn run is told to wrap
// at 15 and a 40-turn run at 30. The prompts used to hardcode those numbers,
// which meant a run configured for 40 turns was told it had 20.
func turnBudget(maxTurns int) (total, wrapAt int) {
	if maxTurns <= 0 {
		maxTurns = 20
	}
	wrapAt = maxTurns - (maxTurns+3)/4
	if wrapAt < 1 {
		wrapAt = 1
	}
	return maxTurns, wrapAt
}

// buildExecutionPrompt constructs the system+user prompt for task execution.
// If context.source_path points to a file (not a directory), the prompt
// directs the worker to read the target file directly in the first turn.
func buildExecutionPrompt(goal, contextStr, workdir string, maxTurns int) string {
	total, wrapAt := turnBudget(maxTurns)

	// Detect file-specific goals from context
	var isFileGoal bool
	if contextStr != "" {
		var ctx map[string]any
		if json.Unmarshal([]byte(contextStr), &ctx) == nil {
			if sp, ok := ctx["source_path"].(string); ok && sp != "" {
				isFileGoal = looksLikeFilePath(sp)
			}
		}
	}

	if isFileGoal {
		return fmt.Sprintf(`You are an autonomous worker agent executing a task on a specific file.

Your job: read and process the target file. The file path is in the CONTEXT below.
Your first command MUST read or inspect the file directly — do NOT search the
filesystem, discover the project, or run find. Start with cat, head, or file.

%s

PROTOCOL — respond with exactly one of these on each turn:
CMD: <shell command>
DONE: <summary of what you accomplished>
FAIL: <reason>

RULES:
- One command per CMD: line. Keep commands focused.
- First command: read the target file directly (cat, head, file, or wc).
- Use head/tail to limit large output.
- Never run destructive commands. Only analysis and inspection tools.
- Commands timeout after 60 seconds.
- You have a budget of %d turns. After turn %d, start wrapping up.
- When you have enough data to answer, DONE: immediately.
- Analyze output before issuing the next command.
- If a command is blocked, try a different approach — don't retry the same one.

AVAILABLE TOOLS: cat, head, tail, wc, grep, file, stat, echo, which

CONTEXT:
%s

GOAL:
%s

Start now: read the target file.`, workingDirectoryBlock(workdir), total, wrapAt, contextStr, goal)
	}

	return fmt.Sprintf(`You are an autonomous worker agent executing a task.

Your job: accomplish the goal using shell commands. You have terminal access
to read-only analysis tools within the repository directory.

%s

PROTOCOL — respond with exactly one of these on each turn:
CMD: <shell command>
DONE: <summary of what you accomplished>
FAIL: <reason>

RULES:
- One command per CMD: line. Keep commands focused.
- Use head/tail to limit large output.
- Never run destructive commands. Only analysis and inspection tools.
- Commands timeout after 60 seconds.
- You have a budget of %d turns. After turn %d, start wrapping up.
- When you have enough data to answer, DONE: immediately.
- Analyze output before issuing the next command.
- If a command is blocked, try a different approach — don't retry the same one.

AVAILABLE TOOLS: find, ls, grep, cat, head, tail, wc, sort, du, file, stat,
diff, cd, pwd, echo, which, go (build/test/vet/fmt/list), golangci-lint,
gosec, govulncheck, awk, sed, python3, node, git (status/log/diff/show).
find with -exec wc -l is allowed and is the fastest way to report file sizes.

CONTEXT:
%s

GOAL:
%s

Start now: output your first CMD: line.`, workingDirectoryBlock(workdir), total, wrapAt, contextStr, goal)
}

// fileExtensions is the set of extensions that indicate a file path rather
// than a directory in goals and context.source_path.
var fileExtensions = map[string]bool{
	".md": true, ".txt": true, ".go": true, ".py": true, ".js": true,
	".ts": true, ".yaml": true, ".yml": true, ".json": true, ".toml": true,
	".cfg": true, ".ini": true, ".env": true, ".proto": true, ".rs": true,
	".c": true, ".h": true, ".cpp": true, ".java": true, ".rb": true,
	".csv": true, ".log": true, ".xml": true, ".html": true, ".css": true,
	".sql": true, ".sh": true, ".ps1": true, ".tf": true, ".hcl": true,
	".mod": true, ".sum": true,
}

// looksLikeFilePath returns true if the path appears to reference a specific
// file rather than a directory or project root.
func looksLikeFilePath(path string) bool {
	path = strings.TrimSpace(path)
	for ext := range fileExtensions {
		if strings.HasSuffix(path, ext) {
			return true
		}
	}
	return false
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
