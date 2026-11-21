package controlplane_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/gen/go/circus/v1/circusv1connect"
	"github.com/EricWvi/circus/internal/auth"
	"github.com/EricWvi/circus/internal/controlplane"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/testutil/wait"
	wc "github.com/EricWvi/circus/internal/testutil/workerclient"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
	"google.golang.org/protobuf/proto"
)

const (
	testPublicURL = "https://circus.example.com"
	testPassword  = "password-123"
	daemonRole    = circusv1.EnrollRole_ENROLL_ROLE_DAEMON
	workerRole    = circusv1.EnrollRole_ENROLL_ROLE_WORKER
	bothRoles     = circusv1.EnrollRole_ENROLL_ROLE_BOTH
)

func (h *harness) machineBrowser(t *testing.T, super bool) (*http.Client, string) {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar, Timeout: 10 * time.Second, Transport: http.DefaultTransport.(*http.Transport).Clone()}
	t.Cleanup(client.CloseIdleConnections)
	api := circusv1connect.NewAuthServiceClient(client, h.base)
	if super {
		match := regexp.MustCompile(`setup\?token=([A-Za-z0-9_-]+)`).FindStringSubmatch(h.logs.String())
		if match == nil {
			t.Fatal("no setup link")
		}
		res, err := api.Setup(t.Context(), connect.NewRequest(&circusv1.SetupRequest{
			Token: match[1], Email: "admin@example.com", Password: testPassword,
		}))
		if err != nil {
			t.Fatal(err)
		}
		return client, res.Msg.GetUser().GetUserId()
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(testPassword), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	u := store.User{ID: "bob", Email: "bob@example.com", Role: store.RoleUser, PasswordHash: string(hash), CreatedAt: time.Now().UnixMilli()}
	if err := h.st.CreateUser(t.Context(), u); err != nil {
		t.Fatal(err)
	}
	if _, err := api.Login(t.Context(), connect.NewRequest(&circusv1.LoginRequest{Email: u.Email, Password: testPassword})); err != nil {
		t.Fatal(err)
	}
	return client, u.ID
}

func issueLink(ctx context.Context, api circusv1connect.MachineServiceClient, role circusv1.EnrollRole) (*connect.Response[circusv1.CreateEnrollLinkResponse], error) {
	return api.CreateEnrollLink(ctx, connect.NewRequest(&circusv1.CreateEnrollLinkRequest{Role: role}))
}

func linkTokens(t *testing.T, link string) []string {
	t.Helper()
	u, err := url.Parse(link)
	if err != nil || u.Scheme+"://"+u.Host != testPublicURL || u.Path != "/join" || u.RawQuery != "" || u.Fragment == "" {
		t.Fatal("invalid enrollment link format")
	}
	tokens := strings.Split(u.Fragment, ".")
	for _, tok := range tokens {
		if !auth.WellFormed(tok) {
			t.Fatal("invalid enrollment token format")
		}
	}
	return tokens
}

func TestWebCreateEnrollLink(t *testing.T) {
	now := time.Now()
	h := newServer(t, controlplane.Config{PublicURL: testPublicURL + "/", Version: "v0.1.0", BcryptCost: bcrypt.MinCost, Now: func() time.Time { return now }}, nil)
	h.machineBrowser(t, true)
	browser, ownerID := h.machineBrowser(t, false)
	api := circusv1connect.NewMachineServiceClient(browser, h.base)
	for _, tc := range []struct {
		role  circusv1.EnrollRole
		kinds []store.PrincipalKind
	}{
		{daemonRole, []store.PrincipalKind{store.KindDevice}},
		{workerRole, []store.PrincipalKind{store.KindWorker}},
		{bothRoles, []store.PrincipalKind{store.KindDevice, store.KindWorker}},
	} {
		t.Run(tc.role.String(), func(t *testing.T) {
			resp, err := issueLink(t.Context(), api, tc.role)
			if err != nil {
				t.Fatal(err)
			}
			tokens := linkTokens(t, resp.Msg.GetLink())
			if len(tokens) != len(tc.kinds) || resp.Msg.GetExpiresUnixMs() != now.Add(auth.DefaultEnrollTTL).UnixMilli() || resp.Msg.GetServerVersion() != "v0.1.0" {
				t.Fatalf("token count %d, expiry %d, version %q", len(tokens), resp.Msg.GetExpiresUnixMs(), resp.Msg.GetServerVersion())
			}
			for i, kind := range tc.kinds {
				role := circusv1.Role_ROLE_DAEMON
				if kind == store.KindWorker {
					role = circusv1.Role_ROLE_WORKER
				}
				req := enrollReq(tokens[i], role, strings.ToLower(tc.role.String()))
				enrolled, err := h.enrollClient().Enroll(t.Context(), req)
				if err != nil {
					t.Fatal(err)
				}
				id, err := h.st.GetIdentity(t.Context(), kind, enrolled.Msg.GetPrincipalId())
				if err != nil || id.UserID != ownerID {
					t.Fatalf("identity owner = %q, error = %v", id.UserID, err)
				}
				if _, err := h.enrollClient().Enroll(t.Context(), req); connect.CodeOf(err) != connect.CodeUnauthenticated {
					t.Fatalf("reused token: %v", err)
				}
				if strings.Contains(h.logs.String(), tokens[i]) {
					t.Fatal("enrollment token appeared in server logs")
				}
			}
		})
	}
}

func TestWebCreateEnrollLinkErrors(t *testing.T) {
	h := newServer(t, controlplane.Config{BcryptCost: bcrypt.MinCost}, nil)
	api := circusv1connect.NewMachineServiceClient(h.http, h.base)
	if _, err := issueLink(t.Context(), api, daemonRole); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("signed out: %v", err)
	}
	browser, _ := h.machineBrowser(t, true)
	api = circusv1connect.NewMachineServiceClient(browser, h.base)
	for _, role := range []circusv1.EnrollRole{circusv1.EnrollRole_ENROLL_ROLE_UNSPECIFIED, 99} {
		if _, err := issueLink(t.Context(), api, role); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Fatalf("invalid role %v: %v", role, err)
		}
	}
	_, err := issueLink(t.Context(), api, daemonRole)
	var rpcErr *connect.Error
	if !errors.As(err, &rpcErr) || rpcErr.Code() != connect.CodeFailedPrecondition || rpcErr.Message() != "public_url is not configured" {
		t.Fatalf("missing public URL: %v", err)
	}
	t.Run("origin and cache policy", func(t *testing.T) {
		h := newServer(t, controlplane.Config{PublicURL: testPublicURL, BcryptCost: bcrypt.MinCost}, nil)
		browser, owner := h.machineBrowser(t, true)
		api := circusv1connect.NewMachineServiceClient(browser, h.base)
		req := connect.NewRequest(&circusv1.CreateEnrollLinkRequest{Role: daemonRole})
		req.Header().Set("Origin", "https://wrong.example.com")
		if _, err := api.CreateEnrollLink(t.Context(), req); connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Fatalf("wrong origin: %v", err)
		}
		if n, err := h.st.CountUnusedEnrollTokens(t.Context(), owner, time.Now().UnixMilli()); err != nil || n != 0 {
			t.Fatalf("wrong origin issued tokens: %d, %v", n, err)
		}
		req = connect.NewRequest(&circusv1.CreateEnrollLinkRequest{Role: daemonRole})
		req.Header().Set("Origin", testPublicURL)
		res, err := api.CreateEnrollLink(t.Context(), req)
		if err != nil {
			t.Fatal(err)
		}
		if got := res.Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("Cache-Control = %q", got)
		}
	})

}

func TestWebEnrollmentTokenCap(t *testing.T) {
	ctx := t.Context()
	now := time.Now()
	h := newServer(t, controlplane.Config{PublicURL: testPublicURL, BcryptCost: bcrypt.MinCost, Now: func() time.Time { return now }}, nil)
	admin, _ := h.machineBrowser(t, true)
	bob, ownerID := h.machineBrowser(t, false)
	api := circusv1connect.NewMachineServiceClient(bob, h.base)
	if _, _, err := auth.CreateEnrollToken(ctx, h.st, ownerID, store.KindDevice, time.Hour, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}

	var token string
	for range 15 {
		resp, err := issueLink(ctx, api, daemonRole)
		if err != nil {
			t.Fatal(err)
		}
		token = linkTokens(t, resp.Msg.GetLink())[0]
		if resp.Msg.GetServerVersion() != "dev" {
			t.Fatalf("fallback version = %q", resp.Msg.GetServerVersion())
		}
	}
	if _, err := issueLink(ctx, api, bothRoles); connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("two tokens with one slot left: %v", err)
	}
	results := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := issueLink(ctx, api, workerRole)
			results <- err
		}()
	}
	errs := []error{<-results, <-results}
	if !slices.Contains(errs, nil) || !slices.ContainsFunc(errs, func(err error) bool { return connect.CodeOf(err) == connect.CodeResourceExhausted }) {
		t.Fatalf("last slot results = %v", errs)
	}
	if _, err := issueLink(ctx, api, daemonRole); connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("at capacity: %v", err)
	}
	if _, err := issueLink(ctx, circusv1connect.NewMachineServiceClient(admin, h.base), bothRoles); err != nil {
		t.Fatalf("another user's allowance: %v", err)
	}
	if _, err := h.enrollClient().Enroll(ctx, enrollReq(token, circusv1.Role_ROLE_DAEMON, "desk")); err != nil {
		t.Fatal(err)
	}
	if _, err := issueLink(ctx, api, daemonRole); err != nil {
		t.Fatalf("used token should free a slot: %v", err)
	}
}

func TestWebMachineOwnership(t *testing.T) {
	h := newServer(t, controlplane.Config{PublicURL: testPublicURL, BcryptCost: bcrypt.MinCost}, nil)
	ctx := wait.Ctx(t)
	admin, adminID := h.machineBrowser(t, true)
	bob, bobID := h.machineBrowser(t, false)
	enroll := func(browser *http.Client, role circusv1.EnrollRole, kind circusv1.Role, name, os string) *circusv1.EnrollResponse {
		t.Helper()
		machines := circusv1connect.NewMachineServiceClient(browser, h.base)
		link, err := issueLink(ctx, machines, role)
		if err != nil {
			t.Fatal(err)
		}
		req := enrollReq(linkTokens(t, link.Msg.GetLink())[0], kind, name)
		req.Msg.Os = os
		res, err := h.enrollClient().Enroll(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		return res.Msg
	}
	enroll(admin, daemonRole, circusv1.Role_ROLE_DAEMON, "desk", "linux")
	enroll(admin, daemonRole, circusv1.Role_ROLE_DAEMON, "admin-only", "linux")
	bobDevice := enroll(bob, daemonRole, circusv1.Role_ROLE_DAEMON, "desk", "windows")
	enroll(bob, daemonRole, circusv1.Role_ROLE_DAEMON, "bob-only", "windows")
	for _, tc := range []struct {
		browser *http.Client
		names   []string
		os      string
	}{
		{admin, []string{"admin-only", "desk"}, "linux"},
		{bob, []string{"bob-only", "desk"}, "windows"},
	} {
		api := circusv1connect.NewMachineServiceClient(tc.browser, h.base)
		res, err := api.ListDevices(ctx, connect.NewRequest(&circusv1.ListDevicesRequest{}))
		if err != nil || len(res.Msg.GetDevices()) != len(tc.names) {
			t.Fatalf("list devices: %v, %v", res, err)
		}
		for i, d := range res.Msg.GetDevices() {
			if d.GetName() != tc.names[i] || d.GetOs() != tc.os {
				t.Fatalf("device = %v, want %s (%s)", d, tc.names[i], tc.os)
			}
		}
	}

	createProject := func(browser *http.Client, device string) (*connect.Response[circusv1.CreateProjectResponse], error) {
		return circusv1connect.NewProjectServiceClient(browser, h.base).CreateProject(ctx, connect.NewRequest(&circusv1.CreateProjectRequest{
			Name: "Project", Locations: []*circusv1.Project_Location{{Device: device}},
		}))
	}
	if _, err := createProject(bob, "admin-only"); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("binding another user's device: %v", err)
	}
	createSession := func(browser *http.Client, ownerID string) (*circusv1.Session, circusv1connect.SessionServiceClient) {
		t.Helper()
		p, err := createProject(browser, "desk")
		if err != nil {
			t.Fatal(err)
		}
		api := circusv1connect.NewSessionServiceClient(browser, h.base)
		res, err := api.CreateSession(ctx, connect.NewRequest(&circusv1.CreateSessionRequest{ClientRequestId: "create-session", ProjectId: p.Msg.GetProject().GetProjectId()}))
		if err != nil {
			t.Fatal(err)
		}
		sess, err := h.st.GetSession(ctx, res.Msg.GetSession().GetSessionId(), time.Now().UnixMilli())
		if err != nil || sess.UserID != ownerID || (ownerID == bobID && sess.DefaultDeviceID != bobDevice.GetPrincipalId()) {
			t.Fatalf("stored session = %+v, error = %v", sess, err)
		}
		return res.Msg.GetSession(), api
	}
	adminSession, adminAPI := createSession(admin, adminID)
	bobSession, bobAPI := createSession(bob, bobID)
	worker := enroll(bob, workerRole, circusv1.Role_ROLE_WORKER, "worker", "linux")
	w, err := wc.Dial(ctx, wc.Options{URL: h.base, Token: worker.GetLongTermToken(), PrincipalID: worker.GetPrincipalId(), BootID: uuid.Must(uuid.NewV7()).String()})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	hints, err := w.Attach(ctx, &circusv1.Attach{MaxSessions: 2})
	if err != nil {
		t.Fatal(err)
	}
	for _, pair := range []struct {
		api circusv1connect.SessionServiceClient
		id  string
	}{{adminAPI, adminSession.GetSessionId()}, {bobAPI, bobSession.GetSessionId()}} {
		if _, err := pair.api.SendMessage(ctx, connect.NewRequest(&circusv1.SendMessageRequest{SessionId: pair.id, ClientMsgId: "msg", Text: "hello"})); err != nil {
			t.Fatal(err)
		}
	}
	hint, err := hints.Next(ctx)
	if err != nil || hint.GetOffer().GetSessionId() != bobSession.GetSessionId() {
		t.Fatalf("worker's offer = %v, error = %v", hint, err)
	}
	// A non-session submit receives the same refusal for either session.
	submitWithoutLease := func(sessionID, callID string) *circusv1.CallUpdate_Rejected {
		t.Helper()
		call, err := w.Submit(ctx, &circusv1.SubmitCall{Device: "desk",
			Call: &circusv1.DeviceCall{CallId: callID, SessionId: sessionID,
				Tool: &circusv1.DeviceCall_Bash{Bash: &circusv1.BashRequest{Command: "true"}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		update, err := call.Next(ctx)
		if err != nil || update.GetRejected().GetCode() != circusv1.CallUpdate_Rejected_CODE_SESSION_FENCED {
			t.Fatalf("submit without lease = %v, error = %v", update, err)
		}
		if _, err := h.st.GetCall(ctx, callID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("rejected call row: %v", err)
		}
		return update.GetRejected()
	}
	own := submitWithoutLease(bobSession.GetSessionId(), "own-session")
	foreign := submitWithoutLease(adminSession.GetSessionId(), "foreign-session")
	if !proto.Equal(own, foreign) {
		t.Fatalf("refusals differ: own %v, foreign %v", own, foreign)
	}

	reply, err := w.Session(ctx, wc.Acquire(adminSession.GetSessionId(), true))
	if err != nil || reply.GetRejected().GetCode() != circusv1.SessionReply_Rejected_CODE_NOT_OFFERED {
		t.Fatalf("lease of another user's session = %v, error = %v", reply, err)
	}
	reply, err = w.Session(ctx, wc.Acquire(bobSession.GetSessionId(), true))
	if err != nil || reply.GetLease().GetEpoch() == 0 {
		t.Fatalf("lease of own session = %v, error = %v", reply, err)
	}
	call, err := w.Submit(ctx, &circusv1.SubmitCall{Device: "admin-only", SessionEpoch: reply.GetLease().GetEpoch(), ToolUseId: "tool",
		Call: &circusv1.DeviceCall{CallId: "foreign-device-call", SessionId: bobSession.GetSessionId(),
			Tool: &circusv1.DeviceCall_Bash{Bash: &circusv1.BashRequest{Command: "true"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	update, err := call.Next(ctx)
	if err != nil || update.GetRejected().GetCode() != circusv1.CallUpdate_Rejected_CODE_UNKNOWN_DEVICE {
		t.Fatalf("call to another user's device = %v, error = %v", update, err)
	}

	// Existing foreign rows must neither count against this session nor
	// disappear when its owner deletes it.
	adminSessionID := adminSession.GetSessionId()
	adminWorker := enroll(admin, workerRole, circusv1.Role_ROLE_WORKER, "admin-worker", "linux")
	now := time.Now().UnixMilli()
	lease, err := h.st.AcquireLease(ctx, store.LeaseRequest{SessionID: adminSessionID,
		WorkerID: adminWorker.GetPrincipalId(), BootID: "boot", ConnID: "conn", Now: now, ExpiresAt: now + time.Minute.Milliseconds()})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.st.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		for i := range 3 {
			call := store.Call{CallID: fmt.Sprintf("foreign-%d", i), UserID: bobID, DeviceID: bobDevice.GetPrincipalId(),
				SessionID: adminSessionID, Tool: "bash", State: store.CallAccepted, RequestHash: []byte("hash"), SubmittedAt: now}
			if err := tx.CreateCall(ctx, call); err != nil {
				return err
			}
		}
		_, err := tx.PutJob(ctx, store.Job{DeviceID: bobDevice.GetPrincipalId(), BootID: "boot", JobID: "foreign-job",
			SessionID: adminSessionID, UserID: bobID, State: store.JobExited, LastSeq: 1})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	fence := store.Fence{SessionID: adminSessionID, Epoch: lease.Lease.Epoch, WorkerID: adminWorker.GetPrincipalId(), BootID: "boot", ConnID: "conn", Now: now}
	if err := h.st.AdmitSessionCall(ctx, fence, "next", 1); err != nil {
		t.Fatalf("foreign rows blocked admission: %v", err)
	}
	if _, err := adminAPI.StopSession(ctx, connect.NewRequest(&circusv1.StopSessionRequest{SessionId: adminSessionID})); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.ReleaseLease(ctx, fence); err != nil {
		t.Fatal(err)
	}
	if err := h.st.CheckSessionDeletable(ctx, adminSessionID, now+1); err != nil {
		t.Fatalf("foreign rows blocked deletion: %v", err)
	}
	if _, err := adminAPI.DeleteSession(ctx, connect.NewRequest(&circusv1.DeleteSessionRequest{SessionId: adminSessionID})); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.GetCall(ctx, "foreign-0"); err != nil {
		t.Fatalf("foreign call after deletion: %v", err)
	}
	jobs, err := h.st.ListJobs(ctx, store.JobFilter{DeviceID: bobDevice.GetPrincipalId(), Limit: 1})
	if err != nil || len(jobs) != 1 || jobs[0].JobID != "foreign-job" {
		t.Fatalf("foreign jobs after deletion: %v, %v", jobs, err)
	}

}

func TestWebMachineIdentityLimits(t *testing.T) {
	h := newServer(t, controlplane.Config{PublicURL: testPublicURL, BcryptCost: bcrypt.MinCost}, nil)
	ctx := t.Context()
	admin, _ := h.machineBrowser(t, true)
	bob, bobID := h.machineBrowser(t, false)
	api := circusv1connect.NewMachineServiceClient(bob, h.base)
	now := time.Now()
	cases := []struct {
		kind  store.PrincipalKind
		role  circusv1.Role
		link  circusv1.EnrollRole
		limit int
	}{
		{store.KindDevice, circusv1.Role_ROLE_DAEMON, daemonRole, auth.MaxLiveDevices},
		{store.KindWorker, circusv1.Role_ROLE_WORKER, workerRole, auth.MaxLiveWorkers},
	}
	for _, tc := range cases {
		if err := h.st.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
			for i := range tc.limit - 1 {
				id := fmt.Sprintf("%s-%d", tc.kind, i)
				if err := tx.CreateIdentity(ctx, store.Identity{Kind: tc.kind, ID: id, UserID: bobID,
					Name: id, MachineID: "m", CreatedAt: now.UnixMilli()}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		last, err := issueLink(ctx, api, tc.link)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.enrollClient().Enroll(ctx, enrollReq(linkTokens(t, last.Msg.GetLink())[0], tc.role, "last")); err != nil {
			t.Fatalf("last %s slot: %v", tc.kind, err)
		}
		if _, err := issueLink(ctx, api, tc.link); connect.CodeOf(err) != connect.CodeResourceExhausted {
			t.Fatalf("full %s issuance: %v", tc.kind, err)
		}

		before, err := h.st.CountUnusedEnrollTokens(ctx, bobID, now.UnixMilli())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := issueLink(ctx, api, bothRoles); connect.CodeOf(err) != connect.CodeResourceExhausted {
			t.Fatalf("BOTH when only %s is full: %v", tc.kind, err)
		}
		if after, err := h.st.CountUnusedEnrollTokens(ctx, bobID, now.UnixMilli()); err != nil || after != before {
			t.Fatalf("BOTH refusal changed token count from %d to %d: %v", before, after, err)
		}

		// CLI issuance can succeed at capacity. Enrollment requires a slot.
		extra, _, err := auth.CreateEnrollToken(ctx, h.st, bobID, tc.kind, time.Hour, now)
		if err != nil {
			t.Fatal(err)
		}
		req := enrollReq(extra, tc.role, "extra")
		_, err = h.enrollClient().Enroll(ctx, req)
		var rpcErr *connect.Error
		want := fmt.Sprintf("live %s limit reached; revoke one first", tc.kind)
		if !errors.As(err, &rpcErr) || rpcErr.Code() != connect.CodeResourceExhausted || rpcErr.Message() != want {
			t.Fatalf("enroll at %s capacity: %v, want RESOURCE_EXHAUSTED with %q", tc.kind, err, want)
		}
		_, err = h.enrollClient().Enroll(ctx, enrollReq(strings.Repeat("A", 43), tc.role, "extra"))
		if !errors.As(err, &rpcErr) || rpcErr.Code() != connect.CodeUnauthenticated || rpcErr.Message() != auth.ErrEnrollRejected.Error() {
			t.Fatalf("invalid token at %s capacity: %v", tc.kind, err)
		}
		logs := h.logs.String()
		if !strings.Contains(logs, "live identity limit reached") || strings.Contains(logs, extra) {
			t.Fatal("missing safe log of the enrollment refusal")
		}

		if _, _, err := auth.RevokeByName(ctx, h.st, tc.kind, bobID, string(tc.kind)+"-0", now); err != nil {
			t.Fatal(err)
		}
		if _, err := h.enrollClient().Enroll(ctx, req); err != nil {
			t.Fatalf("reuse token after freeing a %s slot: %v", tc.kind, err)
		}
		// Keep the next case independent so BOTH is tested with only one
		// role at capacity.
		if _, _, err := auth.RevokeByName(ctx, h.st, tc.kind, bobID, "extra", now); err != nil {
			t.Fatal(err)
		}
	}
	adminLink, err := issueLink(ctx, circusv1connect.NewMachineServiceClient(admin, h.base), bothRoles)
	if err != nil {
		t.Fatalf("another user's issuance allowance: %v", err)
	}
	tokens := linkTokens(t, adminLink.Msg.GetLink())
	for i, tc := range cases {
		if _, err := h.enrollClient().Enroll(ctx, enrollReq(tokens[i], tc.role, "first")); err != nil {
			t.Fatalf("another user's %s allowance: %v", tc.kind, err)
		}
	}
}
