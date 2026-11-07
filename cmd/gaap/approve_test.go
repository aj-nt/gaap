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
}

func (f *fakeApprovalClient) FindReadyTasks(_ context.Context, agentType string, _ int32) ([]*client.TaskEntry, error) {
	if f.failFind {
		return nil, context.DeadlineExceeded
	}
	var out []*client.TaskEntry
	for _, t := range f.ready {
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
	f.completed[taskID] = resultKey
	return &client.TaskEntry{Id: taskID, Status: "done"}, nil
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
