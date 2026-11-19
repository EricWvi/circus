package sessions_test

import (
	"fmt"
	"strings"
	"testing"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/testutil/wait"
	wc "github.com/EricWvi/circus/internal/testutil/workerclient"
)

func bash(callID, session string) *circusv1.DeviceCall {
	return &circusv1.DeviceCall{CallId: callID, SessionId: session, Tool: &circusv1.DeviceCall_Bash{Bash: &circusv1.BashRequest{Command: "true"}}}
}

// submit returns the first update of a SubmitCall.
func submit(t *testing.T, c *wc.Client, sc *circusv1.SubmitCall) *circusv1.CallUpdate {
	t.Helper()
	s, err := c.Submit(wait.Ctx(t), sc)
	must(t, err)
	u, err := s.Next(wait.Ctx(t))
	must(t, err)
	return u
}

func wantCallRejected(t *testing.T, u *circusv1.CallUpdate, code circusv1.CallUpdate_Rejected_Code) {
	t.Helper()
	if u.GetRejected().GetCode() != code {
		t.Fatalf("update %v, want rejected %v", u, code)
	}
}

// TestSubmitCallAdmission: a session call is admitted only on the holder's
// connection at the lease's epoch, with a tool_use_id, while the session
// is not stopped and under its call caps; refusals record nothing.
func TestSubmitCallAdmission(t *testing.T) {
	f := newFixture(t)
	f.session("s1", -1)
	c := f.dial(0, "boot-a")
	ev := f.attach(c, 2)
	f.input("s1", "go", store.InputNormal)
	offerTo(t, ev, "s1")
	epoch := acquire(t, c, "s1")
	recorded := func(id string) bool {
		_, err := f.st.GetCall(t.Context(), id)
		return err == nil
	}

	// A call without session fields for a name that is no session behaves
	// as before (here: the device never connected, so offline).
	if u := submit(t, c, &circusv1.SubmitCall{Device: "desk", Call: bash("legacy", "not-a-session")}); u.GetOffline() == nil {
		t.Fatalf("legacy call %v", u)
	}
	// Without session fields, a real session's call is fenced.
	wantCallRejected(t, submit(t, c, &circusv1.SubmitCall{Device: "desk", Call: bash("c0", "s1")}), circusv1.CallUpdate_Rejected_CODE_SESSION_FENCED)
	wantCallRejected(t, submit(t, c, &circusv1.SubmitCall{Device: "desk", Call: bash("c1", "s1"), SessionEpoch: epoch}), circusv1.CallUpdate_Rejected_CODE_SESSION_FENCED)
	wantCallRejected(t, submit(t, c, &circusv1.SubmitCall{Device: "desk", Call: bash("c2", "s1"), ToolUseId: "tu"}), circusv1.CallUpdate_Rejected_CODE_SESSION_FENCED)
	wantCallRejected(t, submit(t, c, &circusv1.SubmitCall{Device: "desk", Call: bash("c3", "s1"), SessionEpoch: epoch + 1, ToolUseId: "tu"}), circusv1.CallUpdate_Rejected_CODE_SESSION_FENCED)
	wantCallRejected(t, submit(t, c, &circusv1.SubmitCall{Device: "desk", Call: bash("c4", "s1"), SessionEpoch: epoch, ToolUseId: strings.Repeat("t", 129)}), circusv1.CallUpdate_Rejected_CODE_INVALID)
	other := f.dial(1, "boot-b")
	wantCallRejected(t, submit(t, other, &circusv1.SubmitCall{Device: "desk", Call: bash("c5", "s1"), SessionEpoch: epoch, ToolUseId: "tu"}), circusv1.CallUpdate_Rejected_CODE_SESSION_FENCED)
	cli := f.dial(0, "")
	wantCallRejected(t, submit(t, cli, &circusv1.SubmitCall{Device: "desk", Call: bash("c6", "s1"), SessionEpoch: epoch, ToolUseId: "tu"}), circusv1.CallUpdate_Rejected_CODE_SESSION_FENCED)
	for _, id := range []string{"c0", "c1", "c2", "c3", "c4", "c5", "c6"} {
		if recorded(id) {
			t.Fatalf("refused call %s was recorded", id)
		}
	}

	// Admitted: recorded with its tool_use_id (offline: no daemon).
	if u := submit(t, c, &circusv1.SubmitCall{Device: "desk", Call: bash("ok", "s1"), SessionEpoch: epoch, ToolUseId: "tu-ok"}); u.GetOffline() == nil {
		t.Fatalf("admitted call %v", u)
	}
	row, err := f.st.GetCall(t.Context(), "ok")
	must(t, err)
	if row.ToolUseID != "tu-ok" || row.SessionID != "s1" {
		t.Fatalf("row %+v", row)
	}
	// tool_use_id is part of the call's identity; the epoch is not.
	wantCallRejected(t, submit(t, c, &circusv1.SubmitCall{Device: "desk", Call: bash("ok", "s1"), SessionEpoch: epoch, ToolUseId: "tu-other"}), circusv1.CallUpdate_Rejected_CODE_CALL_MISMATCH)
	if u := submit(t, c, &circusv1.SubmitCall{Device: "desk", Call: bash("ok", "s1"), SessionEpoch: epoch, ToolUseId: "tu-ok"}); u.GetOffline() == nil {
		t.Fatalf("resubmitted call %v", u)
	}
	// Attaching to an existing call passes the fence too.
	wantCallRejected(t, submit(t, c, &circusv1.SubmitCall{Device: "desk", Call: bash("ok", "s1"), SessionEpoch: epoch + 1, ToolUseId: "tu-ok"}), circusv1.CallUpdate_Rejected_CODE_SESSION_FENCED)

	// Caps: 16 calls not in a terminal state.
	for i := range 16 {
		must(t, f.st.CreateCall(t.Context(), store.Call{CallID: fmt.Sprintf("busy%d", i), UserID: f.user.ID, DeviceID: f.device.ID, SessionID: "s1",
			Tool: "bash", RequestHash: []byte{1}, RequestBytes: 1, State: store.CallAccepted, SubmittedAt: ms(f)}))
	}
	wantCallRejected(t, submit(t, c, &circusv1.SubmitCall{Device: "desk", Call: bash("over", "s1"), SessionEpoch: epoch, ToolUseId: "tu"}), circusv1.CallUpdate_Rejected_CODE_OVERLOADED)
	if recorded("over") {
		t.Fatal("an overloaded call was recorded")
	}

	// Stopped.
	_, _, _, err = f.st.StopSession(t.Context(), "s1", ms(f), "USER")
	must(t, err)
	wantCallRejected(t, submit(t, c, &circusv1.SubmitCall{Device: "desk", Call: bash("late", "s1"), SessionEpoch: epoch, ToolUseId: "tu"}), circusv1.CallUpdate_Rejected_CODE_SESSION_STOPPED)

	// After a resume on a new connection the old one is fenced.
	f.session("s2", -1)
	f.input("s2", "go", store.InputNormal)
	offerTo(t, ev, "s2")
	e2 := acquire(t, c, "s2")
	c2 := f.dial(0, "boot-a")
	if !call2resume(t, c2, "s2", e2) {
		t.Fatal("resume")
	}
	wantCallRejected(t, submit(t, c, &circusv1.SubmitCall{Device: "desk", Call: bash("old", "s2"), SessionEpoch: e2, ToolUseId: "tu"}), circusv1.CallUpdate_Rejected_CODE_SESSION_FENCED)
	if u := submit(t, c2, &circusv1.SubmitCall{Device: "desk", Call: bash("new", "s2"), SessionEpoch: e2, ToolUseId: "tu"}); u.GetOffline() == nil {
		t.Fatalf("call on the resumed connection %v", u)
	}
}
