package worker

import (
	"context"
	"fmt"
	"time"

	"github.com/aj-nt/gaap/platform"
)

// CommandRunner executes one shell command for a task.
//
// The seam exists because this worker is the one execution path that had none: it
// called `sh -c` unconditionally, on every OS, for the whole life of the product.
// A seam is not a control by itself — what makes it one is which implementation the
// zero value gets.
type CommandRunner interface {
	// Run executes command and returns its captured output.
	Run(ctx context.Context, command, workDir string) cmdOutput
	// Describe names the containment in force, so the startup log says which path
	// will execute rather than leaving it to be inferred from the absence of a line.
	Describe() string
}

// CommandTimeout bounds one command. Unchanged from the pre-seam behaviour.
const CommandTimeout = 60 * time.Second

// HostRunner executes on the host, uncontained. It requires an explicit opt-in:
// see RefuseRunner for why the alternative is the default.
type HostRunner struct{}

func (HostRunner) Run(_ context.Context, command, workDir string) cmdOutput {
	return runCommand(command, workDir)
}

func (HostRunner) Describe() string { return "host, uncoerced (allow_host_exec)" }

// ContainerRunner executes inside the Enforcer's deny-by-default container.
//
// It carries no fallback of its own. If the container cannot run, the command does
// not run on the host instead — that substitution is the whole failure this seam
// exists to prevent, and it is invisible in a log that only records success.
type ContainerRunner struct {
	Enforcer platform.Enforcer
	// UID is the per-agent uid. Not a shared nobody: a shared uid means two agents
	// share a filesystem view, which is a blast radius rather than a boundary.
	UID int
	// Workspace is the single rw mount.
	Workspace string
	Image     string
}

// NewContainerRunner prepares a container-backed runner.
func NewContainerRunner(e platform.Enforcer, uid int, workspace, image string) *ContainerRunner {
	if uid == 0 {
		uid = 10001
	}
	if image == "" {
		image = "alpine:latest"
	}
	return &ContainerRunner{Enforcer: e, UID: uid, Workspace: workspace, Image: image}
}

func (r *ContainerRunner) Run(ctx context.Context, command, workDir string) cmdOutput {
	if r.Enforcer == nil {
		return cmdOutput{ExitCode: -1, Stderr: "BLOCKED: no enforcer; the command was not executed"}
	}
	// workDir is not forwarded: the container has exactly one mount, and honouring a
	// host path here would be a mount the spec never agreed to.
	out, err := r.Enforcer.Run(ctx, platform.NewNamespaceSpec(r.UID, r.Workspace), r.Image, command)
	if err != nil {
		return cmdOutput{Stdout: out, Stderr: fmt.Sprintf("[container]: %v", err), ExitCode: -1}
	}
	return cmdOutput{Stdout: out, ExitCode: 0}
}

func (r *ContainerRunner) Describe() string {
	return fmt.Sprintf("container uid=%d image=%s workspace=%s", r.UID, r.Image, r.Workspace)
}

// RefuseRunner is the default. It executes nothing and says why.
//
// A worker with no containment must not run commands, and the interesting property
// is that refusing is visible: the task fails with a reason that names the missing
// enforcer, rather than the run quietly succeeding on the host under a log line that
// nobody reads. Fail-open is how a config comment claiming "deny by default" ends up
// governing nothing.
type RefuseRunner struct {
	// Why is the operator-facing reason. Required — an unattributed refusal is the
	// defect this whole layer keeps re-learning.
	Why string
}

func (r RefuseRunner) Run(_ context.Context, command, _ string) cmdOutput {
	why := r.Why
	if why == "" {
		why = "no command runner is configured"
	}
	return cmdOutput{
		ExitCode: -1,
		Stderr: fmt.Sprintf("BLOCKED: %s — refusing to execute %q on the host. "+
			"Configure containment (Linux with the gaap dockerd), or pass --allow-host-exec to accept "+
			"uncoerced execution deliberately.", why, truncate(command, 120)),
	}
}

func (r RefuseRunner) Describe() string { return "refuse (fail-closed)" }
