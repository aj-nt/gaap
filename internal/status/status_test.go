// This file is part of Gaap.
// See LICENSE-Apache-2.0 for license information.

package status

import "testing"

// The daemon's task store enforces these exact strings with a CHECK
// constraint, and ExecuteResult JSON is read back by the orchestrator. A
// renamed constant would compile everywhere and fail only against a live
// daemon, so pin the wire vocabulary here.
func TestWireVocabulary(t *testing.T) {
	tasks := map[Task]string{
		TaskBlocked:    "blocked",
		TaskReady:      "ready",
		TaskClaimed:    "claimed",
		TaskDone:       "done",
		TaskFailed:     "failed",
		TaskDeadLetter: "dead_letter",
		TaskSkipped:    "skipped",
	}
	for got, want := range tasks {
		if string(got) != want {
			t.Errorf("task status wire value = %q, want %q", string(got), want)
		}
	}

	results := map[Result]string{
		ResultSuccess: "success",
		ResultFailed:  "failed",
	}
	for got, want := range results {
		if string(got) != want {
			t.Errorf("result status wire value = %q, want %q", string(got), want)
		}
	}
}
