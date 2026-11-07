package worker

import (
	"strings"
	"testing"
)

// TestPromptStatesTheWorkingDirectory is the regression test for a task that
// reported success while analyzing nothing. The prompt never named the working
// directory, so the model assumed /workspace, listed /, found no Go files, and
// returned DONE with an empty finding set — which downstream is indistinguishable
// from "no issues found".
func TestPromptStatesTheWorkingDirectory(t *testing.T) {
	const workdir = "/Users/aj/Documents/repos/gaap"

	prompt := buildExecutionPrompt("inventory the Go files", `{"source_path":"/tmp/x/main.go"}`, workdir, 20)

	if !strings.Contains(prompt, workdir) {
		t.Fatalf("prompt does not name the working directory %q:\n%s", workdir, prompt)
	}
	if !strings.Contains(prompt, "WORKING DIRECTORY") {
		t.Fatal("prompt has no working-directory section for the model to read")
	}
	// The executor sets cmd.Dir, so relative paths are the correct instruction.
	if !strings.Contains(prompt, "relative") {
		t.Fatal("prompt should tell the model to use relative paths")
	}
}

// TestPromptWithoutWorkdirSaysSo: a run with no repo path still must not let the
// model invent one. It is told to find out rather than assume.
func TestPromptWithoutWorkdirSaysSo(t *testing.T) {
	prompt := buildExecutionPrompt("inventory the Go files", "{}", "", 20)

	if !strings.Contains(prompt, "pwd") {
		t.Fatalf("prompt should tell the model to run pwd when the directory is unknown:\n%s", prompt)
	}
	if strings.Contains(prompt, "WORKING DIRECTORY:\n/") {
		t.Fatal("prompt invented a working directory")
	}
}

// TestGenericPromptMentionsWorkdir covers the non-file branch: the two prompts
// are separate strings, and fixing only the file one would leave the common path
// (which is the one that produced the empty analysis) unfixed.
func TestGenericPromptMentionsWorkdir(t *testing.T) {
	const workdir = "/Users/aj/Documents/repos/gaap"

	prompt := buildExecutionPrompt("inventory the Go files", "{}", workdir, 20)

	if !strings.Contains(prompt, workdir) {
		t.Fatalf("generic prompt does not name the working directory:\n%s", prompt)
	}
	if strings.Contains(prompt, "in a specific file") {
		t.Fatal("test is exercising the file-goal prompt; it should exercise the generic one")
	}
}
