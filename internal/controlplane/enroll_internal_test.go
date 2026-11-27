package controlplane

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/coder/websocket"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/gen/go/circus/v1/circusv1connect"
	"github.com/EricWvi/circus/internal/auth"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/store/sqlite"
	"github.com/EricWvi/circus/internal/store/sqlite/sqlitetest"
	"github.com/EricWvi/circus/internal/testutil/wait"
	wc "github.com/EricWvi/circus/internal/testutil/workerclient"
	"github.com/EricWvi/circus/internal/wire"
)

// A rejoin link replaces its identity only while that identity is offline
// by the links that list it online (onlineDevices and the worker
// connections): a ready daemon link or a worker connection refuses it; once
// the link closes, the rejoin replaces the identity, reports it in the
// response, logs it and cuts its credential. A plain join under a taken
// name is refused, pointing to a rejoin, whatever the machine reports.
func TestEnrollRejoinFollowsLinks(t *testing.T) {
	ctx := t.Context()
	db, err := sqlite.Open(ctx, sqlitetest.Path(t), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	logs := &lockedBuffer{}
	s, err := New(ctx, db, Config{Listen: "127.0.0.1:0", DevInsecure: true, LogOutput: logs, BcryptCost: bcrypt.MinCost})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), wait.Timeout)
		defer cancel()
		_ = s.Shutdown(cleanup)
	})
	base := "http://" + s.Addr().String()
	const owner = "owner"
	if err := db.CreateUser(ctx, store.User{ID: owner, Email: "owner@example.com", Role: store.RoleUser, CreatedAt: time.Now().UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 10 * time.Second, Transport: http.DefaultTransport.(*http.Transport).Clone()}
	t.Cleanup(client.CloseIdleConnections)
	enrollRPC := circusv1connect.NewEnrollServiceClient(client, base)
	// enroll enrolls kind with a plain token, or a rejoin token replacing
	// the identity id when given.
	enroll := func(kind store.PrincipalKind, name, machine string, replace ...string) (*circusv1.EnrollResponse, error) {
		t.Helper()
		var token string
		var err error
		if len(replace) > 0 {
			token, _, err = auth.CreateRejoinToken(ctx, db, owner, kind, replace[0], time.Hour, time.Now())
		} else {
			token, _, err = auth.CreateEnrollToken(ctx, db, owner, kind, time.Hour, time.Now())
		}
		if err != nil {
			t.Fatal(err)
		}
		role := circusv1.Role_ROLE_DAEMON
		if kind == store.KindWorker {
			role = circusv1.Role_ROLE_WORKER
		}
		res, err := enrollRPC.Enroll(ctx, connect.NewRequest(&circusv1.EnrollRequest{
			OneTimeToken: token, Role: role, Name: name, Hostname: "h", MachineId: machine, Os: "linux", Arch: "amd64", ClientVersion: "test",
		}))
		if err != nil {
			return nil, err
		}
		return res.Msg, nil
	}
	mustEnroll := func(kind store.PrincipalKind, name, machine string, replace ...string) *circusv1.EnrollResponse {
		t.Helper()
		res, err := enroll(kind, name, machine, replace...)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	refused := func(code connect.Code, kind store.PrincipalKind, name, machine, replace string, want ...string) {
		t.Helper()
		var replaces []string
		if replace != "" {
			replaces = []string{replace}
		}
		_, err := enroll(kind, name, machine, replaces...)
		var ce *connect.Error
		if !errors.As(err, &ce) || ce.Code() != code {
			t.Fatalf("%s %q from %s: %v", kind, name, machine, err)
		}
		for _, w := range want {
			if !strings.Contains(ce.Message(), w) {
				t.Fatalf("%s %q from %s: message %q lacks %q", kind, name, machine, ce.Message(), w)
			}
		}
	}
	waitOnline := func(kind store.PrincipalKind, id string, want bool) {
		t.Helper()
		deadline := wait.Ctx(t)
		for {
			online, err := s.identityOnline(ctx, kind, owner, id)
			if err != nil {
				t.Fatal(err)
			}
			if online == want {
				return
			}
			select {
			case <-deadline.Done():
				t.Fatalf("%s %s never became online=%v", kind, id, want)
			case <-time.After(10 * time.Millisecond):
			}
		}
	}

	dev := mustEnroll(store.KindDevice, "desk", "m")
	wrk := mustEnroll(store.KindWorker, "desk", "m")
	if dev.GetReplaced() != nil || wrk.GetReplaced() != nil {
		t.Fatalf("first enrollments replaced something: %v, %v", dev.GetReplaced(), wrk.GetReplaced())
	}

	// Connect a daemon link and a worker.
	dial := wait.Ctx(t)
	conn, _, err := wire.DialWebSocket(dial, "ws://"+s.Addr().String()+"/v1/link/daemon",
		&websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer " + dev.GetLongTermToken()}}})
	if err != nil {
		t.Fatal(err)
	}
	_, opts, err := wire.DialHello(dial, conn, wire.DaemonUplink, &circusv1.Hello{ProtocolVersion: wire.ProtocolVersion, BootId: "boot-1"}, dev.GetPrincipalId(), wire.Options{})
	if err != nil {
		t.Fatal(err)
	}
	daemon, err := wire.New(conn, wire.DaemonSide, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()
	worker, err := wc.Dial(dial, wc.Options{URL: base, Token: wrk.GetLongTermToken(), PrincipalID: wrk.GetPrincipalId(), BootID: uuid.Must(uuid.NewV7()).String()})
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	waitOnline(store.KindDevice, dev.GetPrincipalId(), true)
	waitOnline(store.KindWorker, wrk.GetPrincipalId(), true)

	// A plain join under the name is refused from any machine, pointing to
	// a rejoin link.
	for _, machine := range []string{"m", "other"} {
		refused(connect.CodeAlreadyExists, store.KindDevice, "DESK", machine, "", `a daemon named "desk" already exists`,
			`"Rejoin…" on that device in the Web UI`, "circus server enroll-token create --user <email> --role daemon --replace <name>", "--name")
	}
	// A rejoin while online: refused, naming the identity and what to do.
	refused(connect.CodeFailedPrecondition, store.KindDevice, "", "m", dev.GetPrincipalId(),
		`daemon "desk" (`+dev.GetPrincipalId()+") is connected now", "Stop it (or delete it)")
	refused(connect.CodeFailedPrecondition, store.KindWorker, "", "m", wrk.GetPrincipalId(), `worker "desk" (`+wrk.GetPrincipalId()+") is connected now")
	// A rejoin under another name: refused.
	refused(connect.CodeInvalidArgument, store.KindDevice, "other-name", "m", dev.GetPrincipalId(), `rejoins as daemon "desk"`)

	// The daemon link closes: the device is offline, so the rejoin replaces
	// it; the worker is still online and keeps its own.
	daemon.Close()
	conn.Close()
	waitOnline(store.KindDevice, dev.GetPrincipalId(), false)
	took := mustEnroll(store.KindDevice, "", "elsewhere", dev.GetPrincipalId())
	if r := took.GetReplaced(); r.GetPrincipalId() != dev.GetPrincipalId() || r.GetName() != "desk" || took.GetName() != "desk" {
		t.Fatalf("replaced = %v, name %q, want %s %q", r, took.GetName(), dev.GetPrincipalId(), "desk")
	}
	refused(connect.CodeFailedPrecondition, store.KindWorker, "", "m", wrk.GetPrincipalId(), "is connected now")
	for _, want := range []string{`"msg":"enrollment replaced identity"`, `"replaced_principal_id":"` + dev.GetPrincipalId() + `"`,
		`"replaced_name":"desk"`, `"revoked_credentials":1`, `"principal_id":"` + took.GetPrincipalId() + `"`} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("log lacks %s:\n%s", want, logs.String())
		}
	}
	if !strings.Contains(logs.String(), `"reason":"rejoin target online"`) || !strings.Contains(logs.String(), `"reason":"name taken"`) {
		t.Fatalf("log lacks the refusals:\n%s", logs.String())
	}
	// The replaced credential is cut like a revoked one: its link upgrade is
	// refused with 403.
	_, resp, err := wire.DialWebSocket(dial, "ws://"+s.Addr().String()+"/v1/link/daemon",
		&websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer " + dev.GetLongTermToken()}}})
	if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden ||
		resp.Header.Get(wire.LinkAuthHeader) != wire.LinkAuthRevoked || resp.Header.Get(wire.LinkPrincipalHeader) != dev.GetPrincipalId() {
		t.Fatalf("replaced credential's upgrade: %v, %v", resp, err)
	}
	// An unknown token proves no revocation: 401, marked only rejected.
	unknown, _ := auth.NewToken()
	_, resp, err = wire.DialWebSocket(dial, "ws://"+s.Addr().String()+"/v1/link/daemon",
		&websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer " + unknown}}})
	if err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized ||
		resp.Header.Get(wire.LinkAuthHeader) != wire.LinkAuthRejected || resp.Header.Get(wire.LinkPrincipalHeader) != "" {
		t.Fatalf("unknown credential's upgrade: %v, %v", resp, err)
	}
	// The token of a rejoin whose identity is gone by now is refused.
	stale, _, err := auth.CreateRejoinToken(ctx, db, owner, store.KindDevice, took.GetPrincipalId(), time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := auth.RevokeByID(ctx, db, store.KindDevice, owner, took.GetPrincipalId(), time.Now()); err != nil {
		t.Fatal(err)
	}
	_, err = enrollRPC.Enroll(ctx, connect.NewRequest(&circusv1.EnrollRequest{OneTimeToken: stale, Role: circusv1.Role_ROLE_DAEMON, Hostname: "h", MachineId: "m"}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "deleted since the link was issued") {
		t.Fatalf("rejoin of a deleted device: %v", err)
	}

	// The worker disconnects, and is replaced in turn.
	if err := worker.Close(); err != nil {
		t.Fatal(err)
	}
	waitOnline(store.KindWorker, wrk.GetPrincipalId(), false)
	if r := mustEnroll(store.KindWorker, "Desk", "m", wrk.GetPrincipalId()).GetReplaced(); r.GetPrincipalId() != wrk.GetPrincipalId() {
		t.Fatalf("worker replaced = %v", r)
	}
}
