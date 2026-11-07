package worker

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

// captureLogs redirects the default slog logger into a buffer for the
// duration of a test. Not parallel-safe — it swaps global state.
func captureLogs(t *testing.T) (*bytes.Buffer, func()) {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})))
	return &buf, func() { slog.SetDefault(prev) }
}

// TestFindExecIsAllowedWhenTheProgramIsSafe is the regression test for the
// run that dead-lettered task_1: the correct one-command answer to
// "inventory the Go files, including sizes" was blocked because the string
// "exec " appeared inside the -exec flag.
func TestFindExecIsAllowedWhenTheProgramIsSafe(t *testing.T) {
	t.Parallel()

	allowed := []string{
		`find . -name "*.go" -type f -exec wc -l {} \; | sort -rn | head -60`,
		`find . -name '*.go' -exec grep -l TODO {} \;`,
		`find internal -type f -execdir head -2 {} \;`,
		`find . -type f -exec file {} \;`,
	}
	for _, cmd := range allowed {
		if err := commandAllowed(cmd); err != nil {
			t.Errorf("%q should be allowed: %v", cmd, err)
		}
	}
}

// TestFindExecIsBlockedWhenTheProgramIsNot verifies the -exec target is what
// gets checked, not the word "-exec" itself.
func TestFindExecIsBlockedWhenTheProgramIsNot(t *testing.T) {
	t.Parallel()

	blocked := []string{
		`find . -type f -exec rm {} \;`,
		`find . -type f -exec mv {} /tmp \;`,
		`find . -type f -exec sh -c 'echo hi' \;`,
		`find . -type f -exec python3 -c 'print(1)' \;`,
		`find . -type f -execdir chmod 777 {} \;`,
		`find . -type f -exec`,
	}
	for _, cmd := range blocked {
		if err := commandAllowed(cmd); err == nil {
			t.Errorf("%q should be blocked", cmd)
		}
	}
}

// TestGrepForDestructiveWordsIsAllowed: a repo that contains a kill switch
// must be searchable for the word "kill". The old substring list made the
// model split the string into 'k'+'i'+'l'+'l' to get past it.
func TestGrepForDestructiveWordsIsAllowed(t *testing.T) {
	t.Parallel()

	allowed := []string{
		`grep -rn "kill switch" internal/`,
		`grep -rn kill internal/approval.go`,
		`grep -rn "mount " .`,
		`grep -rn exec --include=*.go .`,
		`grep -rn "sync " .`,
		`echo "rm -rf / is a destructive command"`,
	}
	for _, cmd := range allowed {
		if err := commandAllowed(cmd); err != nil {
			t.Errorf("%q should be allowed: %v", cmd, err)
		}
	}
}

// TestDestructiveProgramsAreBlocked verifies the allowlist still gates the
// programs themselves, including when given by absolute path.
func TestDestructiveProgramsAreBlocked(t *testing.T) {
	t.Parallel()

	blocked := []string{
		`rm -rf /tmp/foo`,
		`RM -rf /tmp/foo`,
		`pkill -f gaap`,
		`killall gaap-daemon`,
		`kill 1234`,
		`/usr/bin/pkill -f gaap`,
		`chmod 777 internal/worker/executor.go`,
		`curl http://evil.example`,
		`CURL http://evil.example`,
		`wget http://evil.example`,
		`sudo ls`,
		`dd if=/dev/zero of=/tmp/x`,
		`mount /dev/sda1 /mnt`,
		`shutdown -h now`,
		`git push origin main`,
		`git commit -m "x"`,
		`cat /etc/passwd`,
		`cat ~/.ssh/id_rsa`,
	}
	for _, cmd := range blocked {
		if err := commandAllowed(cmd); err == nil {
			t.Errorf("%q should be blocked", cmd)
		}
	}
}

// TestPipingIntoAShellIsBlocked verifies the piped-to program is checked.
func TestPipingIntoAShellIsBlocked(t *testing.T) {
	t.Parallel()

	blocked := []string{
		`echo | sh`,
		`cat x.go | bash`,
		`ls | zsh`,
		`find . -name '*.go' | sh -s`,
	}
	for _, cmd := range blocked {
		if err := commandAllowed(cmd); err == nil {
			t.Errorf("%q should be blocked", cmd)
		}
	}
}

// TestSedInPlaceIsBlocked verifies the "sed is read-only" claim is enforced.
func TestSedInPlaceIsBlocked(t *testing.T) {
	t.Parallel()

	for _, cmd := range []string{
		`sed -i 's/a/b/' main.go`,
		`sed -i.bak 's/a/b/' main.go`,
		`sed --in-place 's/a/b/' main.go`,
	} {
		if err := commandAllowed(cmd); err == nil {
			t.Errorf("%q should be blocked (in-place edit)", cmd)
		}
	}

	for _, cmd := range []string{
		`sed -n '1,40p' main.go`,
		`sed 's|^\./||' files.txt`,
		`sed -e 's/a/b/' main.go`,
	} {
		if err := commandAllowed(cmd); err != nil {
			t.Errorf("%q should be allowed: %v", cmd, err)
		}
	}
}

// TestGitWriteSubcommandsAreBlocked verifies git is read-only by subcommand
// rather than by substring.
func TestGitWriteSubcommandsAreBlocked(t *testing.T) {
	t.Parallel()

	for _, cmd := range []string{
		`git push origin main`,
		`git commit -am "x"`,
		`git tag -a v1 -m x`,
		`git branch -D feature`,
		`git reset --hard HEAD~1`,
		`git checkout main`,
		`git clean -fdx`,
	} {
		if err := commandAllowed(cmd); err == nil {
			t.Errorf("%q should be blocked", cmd)
		}
	}

	for _, cmd := range []string{
		`git status && ls`,
		`git log --oneline -5`,
		`git diff HEAD~1 -- internal/worker/executor.go`,
		`git show HEAD:internal/worker/executor.go | head -20`,
		`git rev-parse --show-toplevel`,
		`git ls-files '*.go'`,
	} {
		if err := commandAllowed(cmd); err != nil {
			t.Errorf("%q should be allowed: %v", cmd, err)
		}
	}
}

// TestDeviceWritesAreBlockedButDevNullIsNot verifies the redirect check
// targets devices rather than the string "/dev/".
func TestDeviceWritesAreBlockedButDevNullIsNot(t *testing.T) {
	t.Parallel()

	for _, cmd := range []string{
		`echo hi > /dev/null`,
		`grep -rn foo . 2>/dev/null`,
		`python3 analyze.py > /dev/null 2>&1`,
	} {
		if err := commandAllowed(cmd); err != nil {
			t.Errorf("%q should be allowed: %v", cmd, err)
		}
	}

	for _, cmd := range []string{
		`echo hi > /dev/sda`,
		`cat x > /dev/rdisk0`,
		`dd if=/dev/zero of=/dev/nvme0n1`,
	} {
		if err := commandAllowed(cmd); err == nil {
			t.Errorf("%q should be blocked", cmd)
		}
	}
}

// TestQuotedSeparatorsAreNotSegments verifies splitter respects quoting.
func TestQuotedSeparatorsAreNotSegments(t *testing.T) {
	t.Parallel()

	allowed := []string{
		`grep -n "a|b" main.go`,
		`sed 's|^\./||' files.txt`,
		`awk -F'\t' '{print $1"|"$2}' inventory.tsv`,
		`grep -rn "foo; bar" .`,
		"grep -rn 'line1\nline2' .",
	}
	for _, cmd := range allowed {
		if err := commandAllowed(cmd); err != nil {
			t.Errorf("%q should be allowed: %v", cmd, err)
		}
	}
}

// TestHeredocBodiesAreNotParsed documents the honest limit: the filter does
// not inspect interpreter bodies, and never could. The old substring scan
// did not stop them either — it only forced the model to obfuscate.
func TestHeredocBodiesAreNotParsed(t *testing.T) {
	t.Parallel()

	cmd := "python3 - <<'PY'\nimport os\nbad = 'k' + 'i' + 'l' + 'l'\nprint('a | b; c')\nPY"
	if err := commandAllowed(cmd); err != nil {
		t.Errorf("multi-line python heredoc should be allowed: %v", err)
	}

	// A heredoc body is data, not commands: a line starting with "rm " there
	// is a string, so it must not be treated as a command segment.
	cmd = "python3 - <<'PY'\nprint('rm -rf /')\nPY"
	if err := commandAllowed(cmd); err != nil {
		t.Errorf("heredoc body must not be split into commands: %v", err)
	}
}

// TestEmptyCommandIsBlocked verifies the empty case is still rejected.
func TestEmptyCommandIsBlocked(t *testing.T) {
	t.Parallel()

	for _, cmd := range []string{"", "   ", "\n"} {
		if err := commandAllowed(cmd); err == nil {
			t.Errorf("%q should be blocked", cmd)
		}
	}
}

// TestWorkLogLinesCarryTaskIdentity verifies each worker log line says which
// task, which attempt and which turn it belongs to. Without this, turn
// accounting in a multi-task log is guesswork.
func TestWorkLogLinesCarryTaskIdentity(t *testing.T) {
	buf, restore := captureLogs(t)
	defer restore()

	mock := &mockChatClient{responses: []string{
		`echo hello`,
		`DONE: done`,
	}}
	exec := NewExecutor(mock, 5)
	task := makeTask("task-identity", "test goal", "test")
	task.RetryCount = 1 // second claim of the same task

	result := exec.Execute(context.Background(), task, "/tmp")
	if result.Status != "success" {
		t.Fatalf("expected success, got %q (%s)", result.Status, result.Error)
	}

	logs := buf.String()
	for _, want := range []string{
		"id=task-identity",
		"attempt=2",
		"turn=1",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("worker log lines should contain %q; got:\n%s", want, logs)
		}
	}
}

// TestUnparseableResponseIsLogged verifies the default branch is visible in
// the log instead of silently burning a turn.
func TestUnparseableResponseIsLogged(t *testing.T) {
	buf, restore := captureLogs(t)
	defer restore()

	mock := &mockChatClient{responses: []string{
		"Let me start by examining the repository structure.",
		`DONE: finished`,
	}}
	exec := NewExecutor(mock, 5)
	task := makeTask("task-drift", "test goal", "test")

	if result := exec.Execute(context.Background(), task, "/tmp"); result.Status != "success" {
		t.Fatalf("expected success, got %q", result.Status)
	}

	logs := buf.String()
	if !strings.Contains(logs, "unparseable") {
		t.Fatalf("unparseable response should be logged; got:\n%s", logs)
	}
	if !strings.Contains(logs, "Let me start by examining") {
		t.Errorf("log should carry what the model actually said; got:\n%s", logs)
	}
	if !strings.Contains(logs, "turn=1") {
		t.Errorf("the unparseable-turn log should name the turn; got:\n%s", logs)
	}
}

// TestPromptStatesTheTurnBudgetItWillEnforce verifies the prompt's budget
// matches the executor's maxTurns instead of being a hardcoded guess.
func TestPromptStatesTheTurnBudgetItWillEnforce(t *testing.T) {
	t.Parallel()

	cases := []struct {
		maxTurns int
		want     string
	}{
		{20, "budget of 20 turns. After turn 15, start wrapping up."},
		{10, "budget of 10 turns. After turn 7, start wrapping up."},
		{40, "budget of 40 turns. After turn 30, start wrapping up."},
		{5, "budget of 5 turns. After turn 3, start wrapping up."},
	}

	for _, c := range cases {
		prompt := buildExecutionPrompt("inventory the Go files", `{}`, "/tmp", c.maxTurns)
		if !strings.Contains(prompt, c.want) {
			t.Errorf("maxTurns=%d: prompt should contain %q", c.maxTurns, c.want)
		}
	}
}

// TestFileGoalPromptUsesTheSameBudget verifies the file-goal prompt no longer
// claims a 10-turn budget while the loop runs 20.
func TestFileGoalPromptUsesTheSameBudget(t *testing.T) {
	t.Parallel()

	prompt := buildExecutionPrompt("summarize it", `{"source_path":"/tmp/x.md"}`, "/tmp", 30)
	if !strings.Contains(prompt, "budget of 30 turns") {
		t.Errorf("file-goal prompt should state the real budget; got:\n%s", prompt)
	}
	if strings.Contains(prompt, "budget of 10 turns") {
		t.Error("file-goal prompt still hardcodes 10 turns")
	}
}
