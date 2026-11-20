package webapi_test

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"
	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/gen/go/circus/v1/circusv1connect"
	"github.com/EricWvi/circus/internal/controlplane/gateway"
	"github.com/EricWvi/circus/internal/controlplane/webapi"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/testutil/wait"
	"google.golang.org/protobuf/proto"
)

func listDirectory(ctx context.Context, api circusv1connect.MachineServiceClient, device, path string) (*connect.Response[circusv1.ListDirectoryResponse], error) {
	return api.ListDirectory(ctx, connect.NewRequest(&circusv1.ListDirectoryRequest{
		Device: device, Path: path, IncludeHidden: true,
	}))
}

func addDirectoryDevice(t *testing.T, h *harness, userID, id, name string) {
	t.Helper()
	if err := h.st.CreateIdentity(t.Context(), store.Identity{
		Kind: store.KindDevice, ID: id, UserID: userID, Name: name, MachineID: "machine-" + id, CreatedAt: h.clock.Load(),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestListDirectoryOwnershipAndPath(t *testing.T) {
	queries := make(chan *circusv1.ListDirectoryQuery, 8)
	sentTo := make(chan string, 8)
	want := &circusv1.ListDirectoryResult{Path: "/home/eric/missing", Parent: "/home/eric", Home: "/home/eric",
		Error: circusv1.DirectoryError_DIRECTORY_ERROR_NOT_FOUND}
	h, super := newHarness(t, func(cfg *webapi.Config) {
		cfg.Query = func(_ context.Context, deviceID string, q *circusv1.DeviceQuery) (*circusv1.DeviceQueryResult, error) {
			queries <- q.GetListDirectory()
			sentTo <- deviceID
			return &circusv1.DeviceQueryResult{Outcome: &circusv1.DeviceQueryResult_ListDirectory{ListDirectory: want}}, nil
		}
	})
	member := h.signIn(t, "member", "member@example.com")
	addDirectoryDevice(t, h, h.owner.ID, "super-desk", "desk")
	addDirectoryDevice(t, h, "member", "member-desk", "desk")
	addDirectoryDevice(t, h, "member", "private-device", "private")
	h.device(t, "revoked", "revoked", "linux")
	if deleted, err := h.st.DeleteIdentity(t.Context(), store.KindDevice, "revoked", h.clock.Load()); err != nil || !deleted {
		t.Fatalf("revoke device: %v, %v", deleted, err)
	}
	ctx := wait.Ctx(t)
	_, err := listDirectory(ctx, h.browser(t).machines, "desk", "")
	wantCode(t, err, connect.CodeUnauthenticated)
	for _, device := range []string{"private", "revoked", "absent"} {
		_, err = listDirectory(ctx, super.machines, device, "")
		wantCode(t, err, connect.CodeNotFound)
	}
	jsonClient := circusv1connect.NewMachineServiceClient(super.http, h.server.URL, connect.WithProtoJSON())
	for _, path := range []string{"/a\x00b", strings.Repeat("\\", 4097)} {
		_, err = listDirectory(ctx, jsonClient, "desk", path)
		wantCode(t, err, connect.CodeInvalidArgument)
		if fieldOf(err) != "path" {
			t.Fatalf("path validation = %v", err)
		}
	}

	escaped := strings.Repeat("\\", 4096)
	for _, tc := range []struct {
		api         circusv1connect.MachineServiceClient
		path, owner string
	}{
		{super.machines, "~/missing", "super-desk"},
		{member.machines, "relative", "member-desk"},
		{jsonClient, escaped, "super-desk"},
	} {
		res, err := listDirectory(ctx, tc.api, "desk", tc.path)
		if err != nil || !proto.Equal(res.Msg.GetResult(), want) {
			t.Fatalf("path-level result = %v, err = %v", res, err)
		}
		if q := wait.Recv(t, queries); q.GetPath() != tc.path || !q.GetIncludeHidden() {
			t.Fatalf("forwarded query = %v", q)
		}
		if device := wait.Recv(t, sentTo); device != tc.owner {
			t.Fatalf("device ID = %q, want %q", device, tc.owner)
		}
	}
	select {
	case q := <-queries:
		t.Fatalf("unexpected query = %v", q)
	default:
	}
}

func TestListDirectoryQueryErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		code circusv1.DeviceQueryError_Code
		want connect.Code
	}{
		{name: "offline", err: gateway.ErrDeviceOffline, want: connect.CodeFailedPrecondition},
		{name: "old daemon", err: gateway.ErrQueryUnsupported, want: connect.CodeUnavailable},
		{name: "link closed", err: gateway.ErrQueryLinkClosed, want: connect.CodeUnavailable},
		{name: "timeout", err: context.DeadlineExceeded, want: connect.CodeDeadlineExceeded},
		{name: "link busy", err: gateway.ErrQueryBusy, want: connect.CodeResourceExhausted},
		{name: "daemon busy", code: circusv1.DeviceQueryError_CODE_BUSY, want: connect.CodeResourceExhausted},
		{name: "shutting down", code: circusv1.DeviceQueryError_CODE_SHUTTING_DOWN, want: connect.CodeUnavailable},
		{name: "invalid", code: circusv1.DeviceQueryError_CODE_INVALID, want: connect.CodeInternal},
		{name: "unsupported query", code: circusv1.DeviceQueryError_CODE_UNSUPPORTED, want: connect.CodeInternal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, browser := newHarness(t, func(cfg *webapi.Config) {
				cfg.Query = func(context.Context, string, *circusv1.DeviceQuery) (*circusv1.DeviceQueryResult, error) {
					return &circusv1.DeviceQueryResult{Outcome: &circusv1.DeviceQueryResult_Error{
						Error: &circusv1.DeviceQueryError{Code: tc.code},
					}}, tc.err
				}
			})
			h.device(t, "device", "desk", "linux")
			_, err := listDirectory(wait.Ctx(t), browser.machines, "desk", "~")
			wantCode(t, err, tc.want)
			if tc.err == gateway.ErrQueryUnsupported && (!strings.Contains(err.Error(), "too old") || !strings.Contains(err.Error(), "update")) {
				t.Fatalf("unsupported message = %v", err)
			}
		})
	}
}

func TestListDirectoryUserCap(t *testing.T) {
	entered := make(chan struct{}, 8)
	release := make(chan struct{})
	h, super := newHarness(t, func(cfg *webapi.Config) {
		cfg.Query = func(ctx context.Context, deviceID string, q *circusv1.DeviceQuery) (*circusv1.DeviceQueryResult, error) {
			if deviceID == "desk-id" && q.GetListDirectory().GetPath() == "~" {
				select {
				case entered <- struct{}{}:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				select {
				case <-release:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			return &circusv1.DeviceQueryResult{Outcome: &circusv1.DeviceQueryResult_ListDirectory{
				ListDirectory: &circusv1.ListDirectoryResult{Path: "/"},
			}}, nil
		}
	})
	h.device(t, "desk-id", "desk", "linux")
	h.device(t, "other-id", "other", "linux")
	member := h.signIn(t, "member", "member@example.com")
	addDirectoryDevice(t, h, "member", "member-desk", "desk")

	ctx := wait.Ctx(t)
	done := make(chan error, 8)
	for range 8 {
		go func() {
			_, err := listDirectory(ctx, super.machines, "desk", "~")
			done <- err
		}()
	}
	for range 8 {
		wait.Recv(t, entered)
	}
	_, err := listDirectory(ctx, super.machines, "other", "/")
	wantCode(t, err, connect.CodeResourceExhausted)
	if _, err := listDirectory(ctx, member.machines, "desk", "/"); err != nil {
		t.Fatalf("other user's query = %v", err)
	}
	close(release)
	for range 8 {
		if err := wait.Recv(t, done); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := listDirectory(ctx, super.machines, "other", "/"); err != nil {
		t.Fatalf("query after slot is freed = %v", err)
	}
}
