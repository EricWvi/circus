package controlplane

import (
	"bytes"
	"context"
	"net/http"
	"net/http/cookiejar"
	"regexp"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/gen/go/circus/v1/circusv1connect"
	"github.com/EricWvi/circus/internal/auth"
	"github.com/EricWvi/circus/internal/controlplane/gateway"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/store/sqlite"
	"github.com/EricWvi/circus/internal/store/sqlite/sqlitetest"
	"github.com/EricWvi/circus/internal/testutil/wait"
	"github.com/EricWvi/circus/internal/wire"
	"github.com/coder/websocket"
	"golang.org/x/crypto/bcrypt"
)

// lockedBuffer collects log lines from concurrent writers.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// A device is online exactly while its daemon link is ready (registered and
// synchronized, so its connection is stored): the server's onlineDevices and
// MachineService.ListDevices follow a real link through connect and close,
// and last_seen follows the stored times.
func TestOnlineDevicesFollowDaemonLink(t *testing.T) {
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

	// Sign in as the super user, who owns the device.
	m := regexp.MustCompile(`setup\?token=([A-Za-z0-9_-]+)`).FindStringSubmatch(logs.String())
	if m == nil {
		t.Fatal("no setup link logged")
	}
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 10 * time.Second, Transport: http.DefaultTransport.(*http.Transport).Clone()}
	t.Cleanup(client.CloseIdleConnections)
	authClient := circusv1connect.NewAuthServiceClient(client, base)
	setup, err := authClient.Setup(ctx, connect.NewRequest(&circusv1.SetupRequest{Token: m[1], Email: "admin@example.com", Password: "password-123"}))
	if err != nil {
		t.Fatal(err)
	}
	ownerID := setup.Msg.GetUser().GetUserId()
	token, _, err := auth.CreateEnrollToken(ctx, db, ownerID, store.KindDevice, time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	e, err := auth.Enroll(ctx, db, auth.EnrollRequest{Token: token, Kind: store.KindDevice, Name: "desk", MachineID: "m", Hostname: "h", OS: "linux", Arch: "amd64", ClientVersion: "1.2.3"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	machines := circusv1connect.NewMachineServiceClient(client, base)
	listDevice := func() *circusv1.Device {
		t.Helper()
		resp, err := machines.ListDevices(ctx, connect.NewRequest(&circusv1.ListDevicesRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		if len(resp.Msg.Devices) != 1 {
			t.Fatalf("devices = %v", resp.Msg.Devices)
		}
		return resp.Msg.Devices[0]
	}
	online := func() bool {
		t.Helper()
		ids, err := s.onlineDevices(ctx, ownerID)
		if err != nil {
			t.Fatal(err)
		}
		return ids[e.Identity.ID]
	}

	if d := listDevice(); d.Online || d.LastSeenUnixMs != 0 || d.Name != "desk" || d.Version != "1.2.3" || online() {
		t.Fatalf("before connecting: %v", d)
	}

	// A route the gateway registered but the router has not yet synchronized
	// is not online: its connection is not recorded, and a link closing now
	// would leave the row with a disconnect and no connect.
	unready := gateway.Connection{Target: gateway.Target{InstanceID: "i", ConnectionID: "c", DeviceID: e.Identity.ID, BootID: "boot-0"}, UserID: ownerID, DeviceName: "desk"}
	if _, err := s.registry.Register(ctx, unready); err != nil {
		t.Fatal(err)
	}
	if d := listDevice(); d.Online || d.LastSeenUnixMs != 0 || online() {
		t.Fatalf("unready route: %v", d)
	}
	if removed, err := s.registry.Unregister(ctx, unready); !removed || err != nil {
		t.Fatal(removed, err)
	}

	// Connect a real daemon link.
	dial := wait.Ctx(t)
	beforeConnect := time.Now().UnixMilli()
	conn, _, err := wire.DialWebSocket(dial, "ws://"+s.Addr().String()+"/v1/link/daemon", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer " + e.Token}}})
	if err != nil {
		t.Fatal(err)
	}
	_, opts, err := wire.DialHello(dial, conn, wire.DaemonUplink, &circusv1.Hello{ProtocolVersion: wire.ProtocolVersion, BootId: "boot-1"}, e.Identity.ID, wire.Options{})
	if err != nil {
		t.Fatal(err)
	}
	l, err := wire.New(conn, wire.DaemonSide, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	for !online() {
		select {
		case <-dial.Done():
			t.Fatal("connected link never became online")
		case <-time.After(10 * time.Millisecond):
		}
	}
	d := listDevice()
	if !d.Online || d.LastSeenUnixMs < beforeConnect || d.LastSeenUnixMs > time.Now().UnixMilli() {
		t.Fatalf("connected: %v (connect at %d)", d, beforeConnect)
	}
	// Another user's id sees nothing of it.
	if ids, err := s.onlineDevices(ctx, "someone-else"); err != nil || len(ids) != 0 {
		t.Fatalf("other user's online devices = %v, %v", ids, err)
	}

	// Close the link: offline, last seen at the recorded disconnect.
	beforeClose := time.Now().UnixMilli()
	l.Close()
	conn.Close()
	for online() {
		select {
		case <-dial.Done():
			t.Fatal("device still online after its link closed")
		case <-time.After(10 * time.Millisecond):
		}
	}
	var row store.Identity
	for {
		row, err = db.GetIdentity(ctx, store.KindDevice, e.Identity.ID)
		if err != nil {
			t.Fatal(err)
		}
		if row.LastDisconnectedAt != 0 {
			break
		}
		select {
		case <-dial.Done():
			t.Fatal("disconnect never recorded")
		case <-time.After(10 * time.Millisecond):
		}
	}
	d = listDevice()
	if d.Online || d.LastSeenUnixMs != row.LastDisconnectedAt || row.LastDisconnectedAt < beforeClose || row.LastConnectedAt < beforeConnect {
		t.Fatalf("after close: %v, row %+v (close at %d)", d, row, beforeClose)
	}
}
