package worker

import (
	"context"
	"strings"
	"testing"
)

// TestParseReplyPlain verifies the three protocol tokens are recognised.
func TestParseReplyPlain(t *testing.T) {
	t.Parallel()

	cases := []struct {
		text string
		kind string
		body string
	}{
		{"CMD: ls -la", actionCmd, "ls -la"},
		{"DONE: found 105 Go files", actionDone, "found 105 Go files"},
		{"FAIL: no go.mod in the tree", actionFail, "no go.mod in the tree"},
		{"  CMD:   ls  ", actionCmd, "ls"},
	}
	for _, c := range cases {
		kind, body, ok := parseReply(c.text)
		if !ok {
			t.Errorf("%q: expected a parsed action", c.text)
			continue
		}
		if kind != c.kind || body != c.body {
			t.Errorf("%q: got (%q, %q), want (%q, %q)", c.text, kind, body, c.kind, c.body)
		}
	}
}

// TestParseReplyStripsTrailingMarkup is the regression test for the run that
// dead-lettered task_2: every command arrived with the model's own tool-call
// closers appended, and the trailing "</" became a command segment that the
// allowlist refused.
func TestParseReplyStripsTrailingMarkup(t *testing.T) {
	t.Parallel()

	cases := []struct {
		text string
		want string
	}{
		{
			"CMD: go list ./... 2>&1 | head -100</parameter>\n</invoke>\n</｜DSML｜",
			"go list ./... 2>&1 | head -100",
		},
		{
			"CMD: cat go.mod</parameter>",
			"cat go.mod",
		},
		{
			"CMD: ls platform</parameter>\n</invoke>\n</antml:function_calls>",
			"ls platform",
		},
		{
			"CMD: find . -name '*.go' | wc -l</br>\n\nWait protocol exact one of these: CMD, DONE, FAIL",
			"find . -name '*.go' | wc -l",
		},
	}
	for _, c := range cases {
		kind, body, ok := parseReply(c.text)
		if !ok || kind != actionCmd {
			t.Errorf("%q: expected a CMD action, got (%q, %q, %v)", c.text, kind, body, ok)
			continue
		}
		if body != c.want {
			t.Errorf("%q:\n  got  %q\n  want %q", c.text, body, c.want)
		}
	}
}

// TestParseReplyStripsLeadingTag verifies a CMD wrapped in the model's own
// parameter tag is still recognised as a command.
func TestParseReplyStripsLeadingTag(t *testing.T) {
	t.Parallel()

	kind, body, ok := parseReply("<parameter name=\"command\">CMD: cat go.mod</parameter>")
	if !ok || kind != actionCmd {
		t.Fatalf("expected a CMD action, got (%q, %q, %v)", kind, body, ok)
	}
	if body != "cat go.mod" {
		t.Errorf("got %q, want %q", body, "cat go.mod")
	}
}

// TestParseReplySkipsNarrationBeforeTheToken recovers the turn that used to be
// spent re-asking: a reply that thinks out loud and then emits a command.
func TestParseReplySkipsNarrationBeforeTheToken(t *testing.T) {
	t.Parallel()

	text := "Let me start by examining the repository structure.\nCMD: ls -la\n"
	kind, body, ok := parseReply(text)
	if !ok || kind != actionCmd {
		t.Fatalf("expected a CMD action, got (%q, %q, %v)", kind, body, ok)
	}
	if body != "ls -la" {
		t.Errorf("got %q, want %q", body, "ls -la")
	}
}

// TestParseReplyKeepsHeredoc verifies a heredoc body survives, including the
// separators that would otherwise look like extra commands.
func TestParseReplyKeepsHeredoc(t *testing.T) {
	t.Parallel()

	text := "CMD: python3 - <<'PY'\nimport os\nprint('a | b; c')\nPY"
	kind, body, ok := parseReply(text)
	if !ok || kind != actionCmd {
		t.Fatalf("expected a CMD action, got (%q, %q, %v)", kind, body, ok)
	}
	want := "python3 - <<'PY'\nimport os\nprint('a | b; c')\nPY"
	if body != want {
		t.Errorf("got\n%q\nwant\n%q", body, want)
	}
}

// TestParseReplyHeredocThenMarkup verifies markup after the heredoc terminator
// does not become part of the command.
func TestParseReplyHeredocThenMarkup(t *testing.T) {
	t.Parallel()

	text := "CMD: python3 - <<'PY'\nprint(1)\nPY</parameter>\n</invoke>"
	_, body, ok := parseReply(text)
	if !ok {
		t.Fatal("expected a CMD action")
	}
	if strings.Contains(body, "parameter") || strings.Contains(body, "invoke") {
		t.Errorf("markup leaked into the command: %q", body)
	}
	if !strings.HasSuffix(body, "PY") {
		t.Errorf("command should end at the heredoc terminator, got %q", body)
	}
}

// TestParseReplyPreservesMultiLineSummary verifies DONE keeps its body.
func TestParseReplyPreservesMultiLineSummary(t *testing.T) {
	t.Parallel()

	text := "DONE: Scanned the repository.\n\n105 Go files, 556,656 bytes total.\n</parameter>"
	kind, body, ok := parseReply(text)
	if !ok || kind != actionDone {
		t.Fatalf("expected a DONE action, got (%q, %q, %v)", kind, body, ok)
	}
	if !strings.Contains(body, "105 Go files") {
		t.Errorf("summary lost its second line: %q", body)
	}
	if strings.Contains(body, "parameter") {
		t.Errorf("markup leaked into the summary: %q", body)
	}
}

// TestParseReplyNoToken verifies a reply with no protocol token is reported as
// unparseable rather than guessed at.
func TestParseReplyNoToken(t *testing.T) {
	t.Parallel()

	for _, text := range []string{
		"",
		"The issue is likely my closing tag being parsed. Let me issue a simpler command.",
		"</parameter>\n</invoke>",
		"I could run a CMD: line here, but I am not sure.",
	} {
		if kind, body, ok := parseReply(text); ok {
			t.Errorf("%q should not parse; got (%q, %q)", text, kind, body)
		}
	}
}

// TestExecuteRunsCommandWithTrailingMarkup verifies the executor applies the
// sanitiser: the command runs instead of being refused for its suffix.
func TestExecuteRunsCommandWithTrailingMarkup(t *testing.T) {
	mock := &mockChatClient{responses: []string{
		"CMD: echo sanitised</parameter>\n</invoke>\n</｜DSML｜",
		"DONE: done",
	}}
	exec := NewExecutor(mock, 5)
	task := makeTask("task-markup", "test goal", "test")

	result := exec.Execute(context.Background(), task, "/tmp")
	if result.Status != "success" {
		t.Fatalf("expected success, got %q (%s)", result.Status, result.Error)
	}
	if _, ok := result.Findings["echo sanitised"]; !ok {
		t.Errorf("expected the command to run and be recorded, findings=%v", result.Findings)
	}
	if _, blocked := result.Findings["echo sanitised</parameter>"]; blocked {
		t.Error("markup should be stripped before the command is recorded")
	}
}

// TestExecuteRunsCommandAfterNarration verifies a thinking-out-loud reply no
// longer burns a turn.
func TestExecuteRunsCommandAfterNarration(t *testing.T) {
	mock := &mockChatClient{responses: []string{
		"Let me look at the package layout first.\nCMD: echo narrated",
		"DONE: done",
	}}
	exec := NewExecutor(mock, 5)
	task := makeTask("task-narration", "test goal", "test")

	result := exec.Execute(context.Background(), task, "/tmp")
	if result.Status != "success" {
		t.Fatalf("expected success, got %q (%s)", result.Status, result.Error)
	}
	if result.LLMTurns != 2 {
		t.Errorf("expected the narrated command to run on turn 1, used %d turns", result.LLMTurns)
	}
	if _, ok := result.Findings["echo narrated"]; !ok {
		t.Errorf("expected the command to run, findings=%v", result.Findings)
	}
}
