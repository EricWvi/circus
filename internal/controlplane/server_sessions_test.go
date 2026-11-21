package controlplane_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptrace"
	"net/url"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/gen/go/circus/v1/circusv1connect"
	"github.com/EricWvi/circus/internal/controlplane"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/testutil/wait"
)

// TestSessionServiceMounted drives SessionService through the real server:
// a session created in a project, a message, a watch stream that sees it,
// and the stream ending UNAVAILABLE when the server shuts down, without
// holding up the shutdown.
func TestSessionServiceMounted(t *testing.T) {
	h := newServer(t, controlplane.Config{}, nil)
	ctx := t.Context()
	var token string
	for line := range strings.SplitSeq(h.logs.String(), "\n") {
		var rec struct {
			URL string `json:"url"`
		}
		if json.Unmarshal([]byte(line), &rec) == nil && rec.URL != "" {
			u, _ := url.Parse(rec.URL)
			token = u.Query().Get("token")
		}
	}
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	t.Cleanup(client.CloseIdleConnections)
	setup, err := circusv1connect.NewAuthServiceClient(client, h.base).Setup(ctx, connect.NewRequest(&circusv1.SetupRequest{Token: token, Email: "admin@example.com", Password: "password-123"}))
	if err != nil {
		t.Fatal(err)
	}
	owner := setup.Msg.GetUser()
	if err := h.st.CreateIdentity(ctx, store.Identity{Kind: store.KindDevice, ID: "d1", UserID: owner.GetUserId(), Name: "box", MachineID: "m1", OS: "linux", CreatedAt: time.Now().UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	project, err := circusv1connect.NewProjectServiceClient(client, h.base).CreateProject(ctx, connect.NewRequest(&circusv1.CreateProjectRequest{
		Name: "Project", Locations: []*circusv1.Project_Location{{Device: "box", Path: "/work"}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	sessions := circusv1connect.NewSessionServiceClient(client, h.base)
	created, err := sessions.CreateSession(ctx, connect.NewRequest(&circusv1.CreateSessionRequest{ClientRequestId: "create-session", ProjectId: project.Msg.Project.ProjectId}))
	if err != nil {
		t.Fatal(err)
	}
	id := created.Msg.Session.SessionId
	sent, err := sessions.SendMessage(ctx, connect.NewRequest(&circusv1.SendMessageRequest{SessionId: id, ClientMsgId: "m1", Text: "hello"}))
	if err != nil || sent.Msg.EventSeq != 1 {
		t.Fatalf("send = %v, %v", sent, err)
	}
	stream, err := sessions.WatchSession(ctx, connect.NewRequest(&circusv1.WatchSessionRequest{SessionId: id}))
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if !stream.Receive() || stream.Msg().Seq != 1 || stream.Msg().Kind != circusv1.SessionEvent_KIND_INPUT_ACCEPTED {
		t.Fatalf("first event = %v, %v", stream.Msg(), stream.Err())
	}
	ended := make(chan error, 1)
	go func() {
		for stream.Receive() {
		}
		ended <- stream.Err()
	}()
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	if err := h.srv.Shutdown(shutdown); err != nil {
		t.Fatalf("Shutdown = %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Shutdown took %v", d)
	}
	if err := wait.Recv(t, ended); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("stream ended with %v", err)
	}
}

// TestWriteDeadlineClearedPerRequest: a request that leaves a write
// deadline on its connection (as SessionService does, to bound its last
// write) does not cut the next request on the same kept-alive connection.
// The top-level handler clears it; net/http (Go 1.26) also clears it after
// every HTTP/1.1 request, so this guards the property, whichever does it.
func TestWriteDeadlineClearedPerRequest(t *testing.T) {
	h := newServer(t, controlplane.Config{}, func(s *controlplane.Server) {
		controlplane.Mount(s, "POST /leaves-deadline", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "first")
			_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(50 * time.Millisecond))
		}))
		controlplane.Mount(s, "POST /plain", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "second")
		}))
	})
	client := &http.Client{Transport: &http.Transport{}}
	t.Cleanup(client.CloseIdleConnections)
	post := func(path string) (string, bool) {
		t.Helper()
		var reused bool
		ctx := httptrace.WithClientTrace(t.Context(), &httptrace.ClientTrace{GotConn: func(i httptrace.GotConnInfo) { reused = i.Reused }})
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.base+path, nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		return string(body), reused
	}
	if body, _ := post("/leaves-deadline"); body != "first" {
		t.Fatalf("first = %q", body)
	}
	time.Sleep(150 * time.Millisecond) // the deadline has passed
	body, reused := post("/plain")
	if body != "second" || !reused {
		t.Fatalf("second = %q on a reused connection: %v", body, reused)
	}
}
