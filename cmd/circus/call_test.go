package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
)

func TestCallOutputAndExitCodes(t *testing.T) {
	for _, tc := range []struct {
		u    *circusv1.CallUpdate
		code int
		text string
	}{
		{&circusv1.CallUpdate{CallId: "id", Update: &circusv1.CallUpdate_Result{Result: &circusv1.DeviceResult{Output: &circusv1.ToolOutput{IsError: true, Content: []*circusv1.ContentBlock{{Block: &circusv1.ContentBlock_Text{Text: "tool error"}}}}}}}, 0, "tool error"},
		{&circusv1.CallUpdate{CallId: "id", Update: &circusv1.CallUpdate_Unknown_{Unknown: &circusv1.CallUpdate_Unknown{}}}, 3, ""},
		{&circusv1.CallUpdate{CallId: "id", Update: &circusv1.CallUpdate_Lost_{Lost: &circusv1.CallUpdate_Lost{}}}, 1, ""},
		{&circusv1.CallUpdate{CallId: "id", Update: &circusv1.CallUpdate_Offline{Offline: &circusv1.CallUpdate_DeviceOffline{}}}, 1, ""},
		{&circusv1.CallUpdate{CallId: "id", Update: &circusv1.CallUpdate_Rejected_{Rejected: &circusv1.CallUpdate_Rejected{}}}, 1, ""},
		{&circusv1.CallUpdate{CallId: "id", Update: &circusv1.CallUpdate_Rejected_{Rejected: &circusv1.CallUpdate_Rejected{Code: circusv1.CallUpdate_Rejected_CODE_NOT_FOUND}}}, 1, ""},
		{&circusv1.CallUpdate{CallId: "id", Update: &circusv1.CallUpdate_Cancelled_{Cancelled: &circusv1.CallUpdate_Cancelled{}}}, 1, ""},
	} {
		var out, errOut bytes.Buffer
		err := printCallUpdate(tc.u, &out, &errOut)
		code := 0
		var e callExit
		if errors.As(err, &e) {
			code = e.code
		} else if err != nil {
			t.Fatal(err)
		}
		if code != tc.code || out.String() != tc.text || !strings.Contains(errOut.String(), `"callId":"id"`) {
			t.Fatalf("code %d stdout %q stderr %q", code, &out, &errOut)
		}
		if tc.code == 3 && !strings.Contains(errOut.String(), "circus call query id") {
			t.Fatal(&errOut)
		}
	}
}

func TestCallValidatesIDsBeforeLoadingCredentials(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--device", "desk", "--call-id", "bad id", "--credentials", "/does/not/exist", "bash", "true"}, "invalid call ID"},
		{[]string{"--device", "desk", "--session", "bad session", "--credentials", "/does/not/exist", "bash", "true"}, "invalid session ID"},
		{[]string{"query", "bad id", "--credentials", "/does/not/exist"}, "invalid call ID"},
		{[]string{"cancel", "bad id", "--credentials", "/does/not/exist"}, "invalid call ID"},
		{[]string{"--device", "desk", "--credentials", "/does/not/exist", "bash", "true"}, "bash requires --cwd"},
	} {
		var out, errOut bytes.Buffer
		err := callCommand(tc.args, strings.NewReader(""), &out, &errOut)
		if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "/does/not/exist") {
			t.Fatalf("%v: %v", tc.args, err)
		}
	}
}

func TestNotFoundAndUsageDocumentExitSemantics(t *testing.T) {
	var out, errOut bytes.Buffer
	u := &circusv1.CallUpdate{CallId: "missing", Update: &circusv1.CallUpdate_Rejected_{Rejected: &circusv1.CallUpdate_Rejected{Code: circusv1.CallUpdate_Rejected_CODE_NOT_FOUND}}}
	err := printCallUpdate(u, &out, &errOut)
	var exit callExit
	if !errors.As(err, &exit) || exit.code != 1 || !strings.Contains(errOut.String(), "unknown call id") {
		t.Fatalf("err=%v stderr=%q", err, errOut.String())
	}
	for _, want := range []string{"--cwd", "--wait", "3 = outcome unknown", "4 = query", "HTTP 503"} {
		if !strings.Contains(callUsage, want) {
			t.Fatalf("usage lacks %q", want)
		}
	}
}
