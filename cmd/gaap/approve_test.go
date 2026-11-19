package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/aj-nt/gaap"
	"github.com/aj-nt/vassago-sdk/client"
	pb "github.com/aj-nt/vassago-sdk/proto"
)

// fakeApprovalClient is the daemon as `gaap approve` sees it: the ready tasks,
// the blackboard, and a place to record completions.
type fakeApprovalClient struct {
	ready       []*client.TaskEntry
	memories    []*pb.MemoryEntry
	completed   map[string]string // task id → result key
	lastWritten string
	failFind    bool

	// claims is the daemon's claimed-by map; events is the ordered record of what
	// the client did, so a test can assert the claim happens before any write.
	claims    map[string]string
	tasks     map[string]*client.TaskEntry // what GetTask returns
	claimLost bool                         // another approver won the race
	events    []string
}

func (f *fakeApprovalClient) FindReadyTasks(_ context.Context, agentType string, _ int32) ([]*client.TaskEntry, error) {
	if f.failFind {
		return nil, context.DeadlineExceeded
	}
	var out []*client.TaskEntry
	for _, t := range f.ready {
		// The daemon's query is WHERE status = 'ready', so a claimed question is not
		// listed at all. Modelling it as "return everything" let a test prove the
		// retry branch while the CLI could never reach it.
		if t.Status != "ready" {
			continue
		}
		if t.AgentType == agentType {
			out = append(out, t)
		}
	}
	return out, nil
}

func (f *fakeApprovalClient) AddMemory(_ context.Context, _, _, key, content string, _ int32, _ string) (*pb.MemoryEntry, error) {
	f.lastWritten = content
	entry := &pb.MemoryEntry{Id: "mem-" + key, Key: key, Content: content}
	f.memories = append(f.memories, entry)
	return entry, nil
}

func (f *fakeApprovalClient) CompleteTask(_ context.Context, taskID, resultKey string) (*client.TaskEntry, error) {
	if f.completed == nil {
		f.completed = map[string]string{}
	}
	f.events = append(f.events, "complete:"+taskID)
	f.completed[taskID] = resultKey
	return &client.TaskEntry{Id: taskID, Status: "done"}, nil
}

// ClaimTask mirrors the daemon: the CAS only succeeds while the task is ready, and
// a loss comes back as the daemon's sentinel entry (status claimed, owned by
// "other") rather than an error.
func (f *fakeApprovalClient) ClaimTask(_ context.Context, taskID, agentID string) (*client.TaskEntry, error) {
	f.events = append(f.events, "claim:"+taskID+":"+agentID)
	if f.claimLost || f.claims[taskID] != "" {
		return &client.TaskEntry{Id: taskID, Status: "claimed", AssignedAgent: "other"}, nil
	}
	if f.claims == nil {
		f.claims = map[string]string{}
	}
	f.claims[taskID] = agentID
	return &client.TaskEntry{Id: taskID, Status: "claimed", AssignedAgent: agentID}, nil
}

func (f *fakeApprovalClient) GetTask(_ context.Context, id string) (*client.TaskEntry, error) {
	return f.tasks[id], nil
}

// pendingTask builds an approval task as the orchestrator would leave it.
func pendingTask(approvalTaskID, failedTaskID, agentType string, attempts int32) *client.TaskEntry {
	req := gaap.ApprovalRequest{
		TaskID:    failedTaskID,
		AgentType: agentType,
		Attempts:  attempts,
		Reason:    "daemon dead-lettered the task after 2 attempt(s)",
		OpenedAt:  time.Now().Add(-time.Minute),
		Deadline:  time.Now().Add(time.Minute),
	}
	body, _ := json.Marshal(req)
	return &client.TaskEntry{
		Id:        approvalTaskID,
		AgentType: gaap.ApprovalAgentType,
		Goal:      "approve " + failedTaskID,
		Context:   string(body),
		Status:    "ready",
	}
}

func TestApprovalsListsOnlyTheNamedRun(t *testing.T) {
	c := &fakeApprovalClient{ready: []*client.TaskEntry{
		pendingTask("abc12345_scan_approval", "abc12345_scan", "quality_scan", 2),
		pendingTask("def67890_scan_approval", "def67890_scan", "quality_scan", 2),
	}}

	var out strings.Builder
	if err := approvalsRun(context.Background(), c, &approvalsConfig{RunID: "abc12345"}, &out); err != nil {
		t.Fatalf("approvalsRun: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "abc12345_scan") {
		t.Errorf("output should list the run's request:\n%s", got)
	}
	if strings.Contains(got, "def67890") {
		t.Errorf("output should not list another run's request:\n%s", got)
	}
}

func TestApprovalsIsEmptyForARunWithNone(t *testing.T) {
	c := &fakeApprovalClient{ready: []*client.TaskEntry{pendingTask("abc12345_scan_approval", "abc12345_scan", "quality_scan", 2)}}

	var out strings.Builder
	if err := approvalsRun(context.Background(), c, &approvalsConfig{RunID: "zzz99999"}, &out); err != nil {
		t.Fatalf("an empty list is not an error: %v", err)
	}
	if !strings.Contains(out.String(), "no pending approvals") {
		t.Errorf("output should say there are none:\n%s", out.String())
	}
}

func TestApprovalsJSON(t *testing.T) {
	c := &fakeApprovalClient{ready: []*client.TaskEntry{pendingTask("abc12345_scan_approval", "abc12345_scan", "quality_scan", 2)}}

	var out strings.Builder
	if err := approvalsRun(context.Background(), c, &approvalsConfig{RunID: "abc12345", JSON: true}, &out); err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		ApprovalTask string `json:"approval_task"`
		TaskID       string `json:"task_id"`
		AgentType    string `json:"agent_type"`
		Attempts     int32  `json:"attempts"`
	}
	if err := json.Unmarshal([]byte(out.String()), &rows); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out.String())
	}
	if len(rows) != 1 || rows[0].TaskID != "abc12345_scan" || rows[0].AgentType != "quality_scan" || rows[0].Attempts != 2 {
		t.Errorf("rows = %+v", rows)
	}
}

func TestApprovePicksTheOnlyRequest(t *testing.T) {
	c := &fakeApprovalClient{ready: []*client.TaskEntry{
		pendingTask("abc12345_scan_approval", "abc12345_scan", "quality_scan", 2),
	}}

	var out strings.Builder
	cfg := &approveConfig{RunID: "abc12345", Action: "retry", Reason: "rate limited", Agent: "aj"}
	if err := approveRun(context.Background(), c, cfg, &out); err != nil {
		t.Fatalf("approveRun: %v", err)
	}

	if _, ok := c.completed["abc12345_scan_approval"]; !ok {
		t.Fatalf("the approval task was not completed: %v", c.completed)
	}
	var d gaap.ApprovalDecision
	if err := json.Unmarshal([]byte(c.lastWritten), &d); err != nil {
		t.Fatalf("the decision written to the blackboard is not JSON: %v (%q)", err, c.lastWritten)
	}
	if d.Action != gaap.ApprovalRetry || d.Reason != "rate limited" || d.DecidedBy != "aj" {
		t.Errorf("decision = %+v", d)
	}
	if d.DecidedAt.IsZero() {
		t.Error("the decision should record when it was made")
	}
	if !strings.Contains(out.String(), "abc12345_scan") {
		t.Errorf("the operator should see what they decided about:\n%s", out.String())
	}
}

func TestApproveRefusesAmbiguity(t *testing.T) {
	c := &fakeApprovalClient{ready: []*client.TaskEntry{
		pendingTask("abc12345_a_approval", "abc12345_a", "quality_scan", 2),
		pendingTask("abc12345_b_approval", "abc12345_b", "static_analysis", 2),
	}}

	var out strings.Builder
	cfg := &approveConfig{RunID: "abc12345", Action: "retry", Reason: "r", Agent: "aj"}
	err := approveRun(context.Background(), c, cfg, &out)
	if err == nil {
		t.Fatal("two pending requests and no --task should be an error, not a guess")
	}
	if !strings.Contains(err.Error(), "--task") {
		t.Errorf("the error should say how to disambiguate: %v", err)
	}
	if len(c.completed) != 0 {
		t.Error("nothing should have been answered")
	}
}

func TestApproveSelectsByTask(t *testing.T) {
	c := &fakeApprovalClient{ready: []*client.TaskEntry{
		pendingTask("abc12345_a_approval", "abc12345_a", "quality_scan", 2),
		pendingTask("abc12345_b_approval", "abc12345_b", "static_analysis", 2),
	}}

	var out strings.Builder
	cfg := &approveConfig{RunID: "abc12345", Task: "abc12345_b", Action: "cancel", Reason: "not worth it", Agent: "aj"}
	if err := approveRun(context.Background(), c, cfg, &out); err != nil {
		t.Fatalf("approveRun: %v", err)
	}
	if _, ok := c.completed["abc12345_b_approval"]; !ok {
		t.Errorf("the named request was not answered: %v", c.completed)
	}
	if _, ok := c.completed["abc12345_a_approval"]; ok {
		t.Error("the other request was answered too")
	}
}

func TestApproveRejectsAnUnknownTask(t *testing.T) {
	c := &fakeApprovalClient{ready: []*client.TaskEntry{
		pendingTask("abc12345_a_approval", "abc12345_a", "quality_scan", 2),
	}}

	var out strings.Builder
	cfg := &approveConfig{RunID: "abc12345", Task: "abc12345_zzz", Action: "retry", Reason: "r", Agent: "aj"}
	if err := approveRun(context.Background(), c, cfg, &out); err == nil {
		t.Fatal("an unknown task should be an error")
	}
}

func TestApproveRejectsAnAgentOutsideTheAllowlist(t *testing.T) {
	c := &fakeApprovalClient{ready: []*client.TaskEntry{
		pendingTask("abc12345_a_approval", "abc12345_a", "quality_scan", 2),
	}}

	var out strings.Builder
	cfg := &approveConfig{
		RunID: "abc12345", Action: "retry", Reason: "r",
		Agent: "mallory", Agents: []string{"aj", "cait"},
	}
	err := approveRun(context.Background(), c, cfg, &out)
	if err == nil {
		t.Fatal("an agent outside the allowlist should be refused")
	}
	if !strings.Contains(err.Error(), "mallory") {
		t.Errorf("the error should name the agent: %v", err)
	}
	if len(c.completed) != 0 {
		t.Error("nothing should have been answered")
	}
}

func TestApproveRequiresAReason(t *testing.T) {
	c := &fakeApprovalClient{ready: []*client.TaskEntry{
		pendingTask("abc12345_a_approval", "abc12345_a", "quality_scan", 2),
	}}

	var out strings.Builder
	cfg := &approveConfig{RunID: "abc12345", Action: "retry", Agent: "aj"}
	if err := approveRun(context.Background(), c, cfg, &out); err == nil {
		t.Fatal("a decision with no reason is not auditable and should be refused")
	}
}

func TestApproveRejectsAgentTypeWithoutReassign(t *testing.T) {
	c := &fakeApprovalClient{ready: []*client.TaskEntry{
		pendingTask("abc12345_a_approval", "abc12345_a", "quality_scan", 2),
	}}

	var out strings.Builder
	cfg := &approveConfig{RunID: "abc12345", Action: "retry", AgentType: "file_analysis", Reason: "r", Agent: "aj"}
	err := approveRun(context.Background(), c, cfg, &out)
	if err == nil {
		t.Fatal("--agent-type without --action reassign would be silently ignored")
	}
	if !strings.Contains(err.Error(), "reassign") {
		t.Errorf("the error should point at the reassign action: %v", err)
	}
}

// The claim is the serialization point: if two operators answer the same question
// at the same time, the daemon's CAS lets exactly one of them through. Without it
// both write a decision record and both complete the task, and the second result
// key silently overwrites the first.
func TestApproveClaimsTheRequestBeforeRecordingAnything(t *testing.T) {
	c := &fakeApprovalClient{ready: []*client.TaskEntry{
		pendingTask("abc12345_a_approval", "abc12345_a", "quality_scan", 2),
	}}

	var out strings.Builder
	cfg := &approveConfig{RunID: "abc12345", Action: "retry", Reason: "rate limited", Agent: "aj"}
	if err := approveRun(context.Background(), c, cfg, &out); err != nil {
		t.Fatalf("approveRun: %v", err)
	}

	if len(c.events) == 0 || !strings.HasPrefix(c.events[0], "claim:abc12345_a_approval:aj") {
		t.Fatalf("the claim must be the first thing the approver does, got %v", c.events)
	}
	if got := c.claims["abc12345_a_approval"]; got != "aj" {
		t.Errorf("the claim should be held by the approver, got %q", got)
	}
	if _, ok := c.completed["abc12345_a_approval"]; !ok {
		t.Errorf("the winning approver should still complete the task: %v", c.completed)
	}
}

func TestApproveRefusesWhenAnotherApproverAnsweredFirst(t *testing.T) {
	c := &fakeApprovalClient{
		ready: []*client.TaskEntry{
			pendingTask("abc12345_a_approval", "abc12345_a", "quality_scan", 2),
		},
		claimLost: true,
		tasks: map[string]*client.TaskEntry{
			"abc12345_a_approval": {Id: "abc12345_a_approval", Status: "done", AssignedAgent: "sam"},
		},
	}

	var out strings.Builder
	cfg := &approveConfig{RunID: "abc12345", Action: "retry", Reason: "r", Agent: "aj"}
	err := approveRun(context.Background(), c, cfg, &out)
	if err == nil {
		t.Fatal("a question another operator already answered must not be answered again")
	}
	if !strings.Contains(err.Error(), "already") {
		t.Errorf("the error should say the question was already answered: %v", err)
	}
	if len(c.memories) != 0 {
		t.Errorf("a losing approver must not write a decision to the blackboard: %d written", len(c.memories))
	}
	if len(c.completed) != 0 {
		t.Errorf("a losing approver must not complete the task: %v", c.completed)
	}
}

// A failed completion after a successful claim leaves the task claimed by us. The
// retry has to go through, or a transient daemon error locks the question forever.
func TestApproveProceedsWhenWeAlreadyHoldTheClaim(t *testing.T) {
	// Claimed by us, completion never landed: the question is not ready, so it is
	// absent from FindReadyTasks (the daemon filters status = 'ready'). Naming it is
	// the only way back to it, and the claim we hold is what lets the retry through
	// instead of reporting the task as held by another operator.
	claimed := pendingTask("abc12345_a_approval", "abc12345_a", "quality_scan", 2)
	claimed.Status = "claimed"
	claimed.AssignedAgent = "aj"
	c := &fakeApprovalClient{
		claims: map[string]string{"abc12345_a_approval": "aj"},
		tasks:  map[string]*client.TaskEntry{"abc12345_a_approval": claimed},
	}

	var out strings.Builder
	cfg := &approveConfig{RunID: "abc12345", Task: "abc12345_a_approval", Action: "retry", Reason: "rate limited", Agent: "aj"}
	if err := approveRun(context.Background(), c, cfg, &out); err != nil {
		t.Fatalf("retrying our own claim should succeed: %v", err)
	}
	if _, ok := c.completed["abc12345_a_approval"]; !ok {
		t.Errorf("the retry should complete the task: %v", c.completed)
	}
	if _, ok := c.claims["abc12345_a_approval"]; !ok {
		t.Errorf("the retry should still hold the claim: %v", c.claims)
	}
}

// A named task that belongs to a different run is refused: the run-id prefix is the
// only thing tying a question to the run that asked it, and the direct read reaches
// tasks the ready list would never have offered.
func TestApproveRejectsAnApprovalFromAnotherRun(t *testing.T) {
	other := pendingTask("def67890_a_approval", "def67890_a", "quality_scan", 2)
	c := &fakeApprovalClient{
		tasks: map[string]*client.TaskEntry{"def67890_a_approval": other},
	}

	var out strings.Builder
	cfg := &approveConfig{RunID: "abc12345", Task: "def67890_a_approval", Action: "retry", Reason: "r", Agent: "aj"}
	err := approveRun(context.Background(), c, cfg, &out)
	if err == nil {
		t.Fatal("a question belonging to another run must not be answered")
	}
	if !strings.Contains(err.Error(), "does not belong to run") {
		t.Errorf("the error should name the mismatch: %v", err)
	}
	if len(c.memories) != 0 || len(c.completed) != 0 {
		t.Errorf("nothing should be written: memories=%v completed=%v", c.memories, c.completed)
	}
}
