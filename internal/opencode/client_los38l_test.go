package opencode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// --- YM-309 (Los 38l): two stamps the idle guard read as work ---
//
//  1. a completion stamp in the future: every read counts it as a turn completed
//     after the cutoff, so the idle guard's "did the agent answer" question is
//     answered yes forever and the refire accounting restarts instead of
//     escalating (security finding: same UID can write that stamp);
//  2. a background shell job: the tool part of the newest turn is already
//     completed, its metadata carries status "running" and a shell id, and the
//     session reports no open turn. The agent ended its turn to wait for the job
//     notification and looked idle for the whole run (live 18.10., agents
//     agent-20261018-24 and agent-20261018-30).

// serveActivity encodes one message page for the idle guard's session read.
func serveActivity(t *testing.T, messages []map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"data": messages})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A settled turn whose completion stamp lies in the future is not evidence that
// the agent answered: the stamp cannot age out of the cutoff window, so it would
// report work on every read for as long as the turn stays in the newest page.
func TestLos38l_FutureCompletionStampIsNotEmptyWork(t *testing.T) {
	future := time.Now().Add(time.Hour)
	srv := serveActivity(t, []map[string]any{
		{"id": "msg_4", "type": "assistant", "finish": "stop",
			"time": map[string]any{"created": future.Add(-time.Second).UnixMilli(), "completed": future.UnixMilli()}},
		{"id": "msg_3", "type": "assistant", "finish": "stop",
			"time": map[string]any{"created": future.Add(-time.Minute).UnixMilli(), "completed": future.Add(-30 * time.Second).UnixMilli()}},
	})

	got, err := testClient(srv).AssistantActivitySince(context.Background(), "ses_1", time.Now().Add(-10*time.Minute))
	if err != nil {
		t.Fatalf("AssistantActivitySince: %v", err)
	}
	if got.Since != 0 {
		t.Errorf("Since = %d, want 0: a stamp in the future is not a completed turn since the relay", got.Since)
	}
	if got.Working {
		t.Error("Working = true, want false: a future completion stamp says nothing about a running turn")
	}
}

// A small clock difference between service and daemon is not a lie: a turn
// completed a few seconds ahead of the daemon's clock still counts.
func TestLos38l_SmallClockSkewStillCounts(t *testing.T) {
	now := time.Now()
	srv := serveActivity(t, []map[string]any{
		{"id": "msg_2", "type": "assistant", "finish": "stop",
			"time": map[string]any{"created": now.Add(-time.Minute).UnixMilli(), "completed": now.Add(5 * time.Second).UnixMilli()}},
	})

	got, err := testClient(srv).AssistantActivitySince(context.Background(), "ses_1", now.Add(-10*time.Minute))
	if err != nil {
		t.Fatalf("AssistantActivitySince: %v", err)
	}
	if got.Since != 1 {
		t.Errorf("Since = %d, want 1: a completion a few seconds ahead is clock skew, not a future stamp", got.Since)
	}
}

// The shell tool moves a background command into a job and returns at once with
// status "running" in its metadata; the tool part itself is completed and the
// assistant turn is settled. That job is work while it runs.
func TestLos38l_BackgroundShellJobIsWork(t *testing.T) {
	started := time.Now().Add(-4 * time.Minute)
	srv := serveActivity(t, []map[string]any{
		{"id": "msg_2", "type": "assistant", "finish": "tool-calls",
			"content": []map[string]any{{
				"type": "tool", "name": "bash",
				"state": map[string]any{
					"status":   "completed",
					"metadata": map[string]any{"status": "running", "shellID": "sh_1"},
				},
			}},
			"time": map[string]any{"created": started.UnixMilli(), "completed": started.Add(time.Second).UnixMilli()}},
	})

	got, err := testClient(srv).AssistantActivitySince(context.Background(), "ses_1", time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatalf("AssistantActivitySince: %v", err)
	}
	if !got.Working {
		t.Error("Working = false, want true: a background shell job is still running")
	}
	if got.WorkingSince.IsZero() || got.WorkingSince.Sub(started).Abs() > time.Second {
		t.Errorf("WorkingSince = %v, want the turn's own start %v", got.WorkingSince, started)
	}
}

// A finished job's metadata keeps saying "running", so the job must be bounded by
// age: the reader hands the turn's own start through unchanged, and the caller
// (workingTurnCounts) decides what age still counts. A 45-minute-old job is
// therefore still "Working" here and bounded there — the opposite of what the
// first sentence of this comment used to claim (5.2 finding 7).
func TestLos38l_OldBackgroundShellJobStillCarriesItsStart(t *testing.T) {
	started := time.Now().Add(-45 * time.Minute)
	srv := serveActivity(t, []map[string]any{
		{"id": "msg_2", "type": "assistant", "finish": "tool-calls",
			"content": []map[string]any{{
				"type": "tool", "name": "bash",
				"state": map[string]any{
					"status":   "completed",
					"metadata": map[string]any{"status": "running", "shellID": "sh_2"},
				},
			}},
			"time": map[string]any{"created": started.UnixMilli(), "completed": started.Add(time.Second).UnixMilli()}},
	})

	got, err := testClient(srv).AssistantActivitySince(context.Background(), "ses_1", time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatalf("AssistantActivitySince: %v", err)
	}
	if !got.Working {
		t.Fatal("Working = false, want true: the reader reports the job, the caller bounds its age")
	}
	if got.WorkingSince.Sub(started).Abs() > time.Second {
		t.Errorf("WorkingSince = %v, want the turn's own start %v (the caller bounds the age with it)",
			got.WorkingSince, started)
	}
}

// A stamp in the future also says nothing about when the turn started. The
// caller bounds a working turn by that stamp, so it is handed over as unknown
// (zero) and the caller's own window decides (5.3 finding 4).
func TestLos38l_FutureCreatedStampIsHandedOverAsUnknown(t *testing.T) {
	future := time.Now().Add(time.Hour)
	srv := serveActivity(t, []map[string]any{
		{"id": "msg_2", "type": "assistant",
			"time": map[string]any{"created": future.UnixMilli(), "completed": 0}},
	})

	got, err := testClient(srv).AssistantActivitySince(context.Background(), "ses_1", time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatalf("AssistantActivitySince: %v", err)
	}
	if !got.Working {
		t.Fatal("Working = false, want true: the turn is open")
	}
	if !got.WorkingSince.IsZero() {
		t.Errorf("WorkingSince = %v, want zero: a start stamp in the future is not a start", got.WorkingSince)
	}
}

// The job signal belongs to the turn the agent stopped in. Once the agent has
// answered the job notification with a newer turn, the old job's metadata must
// not hold the guard off (5.3 finding 1).
func TestLos38l_JobInAnOlderTurnDoesNotCount(t *testing.T) {
	jobTurn := time.Now().Add(-5 * time.Minute)
	srv := serveActivity(t, []map[string]any{
		{"id": "msg_3", "type": "assistant", "finish": "stop",
			"time": map[string]any{"created": jobTurn.Add(2 * time.Minute).UnixMilli(), "completed": jobTurn.Add(2*time.Minute + time.Second).UnixMilli()}},
		{"id": "msg_2", "type": "assistant", "finish": "tool-calls",
			"content": []map[string]any{{
				"type": "tool", "name": "shell",
				"state": map[string]any{
					"status":   "completed",
					"metadata": map[string]any{"status": "running", "shellID": "sh_3"},
				},
			}},
			"time": map[string]any{"created": jobTurn.UnixMilli(), "completed": jobTurn.Add(time.Second).UnixMilli()}},
	})

	got, err := testClient(srv).AssistantActivitySince(context.Background(), "ses_1", time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatalf("AssistantActivitySince: %v", err)
	}
	if got.Working {
		t.Error("Working = true, want false: the job belongs to an older turn the agent has moved past")
	}
}
