//go:build e2e && linux

package e2e

import (
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"testing"

	"connectrpc.com/connect"
	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/gen/go/circus/v1/circusv1connect"
	testwait "github.com/EricWvi/circus/internal/testutil/wait"
	"google.golang.org/protobuf/proto"
)

func TestListDirectoryOnDaemon(t *testing.T) {
	h := newHarness(t)
	dir := filepath.Join(h.dir, "browse")
	must(t, os.MkdirAll(filepath.Join(dir, "project", ".git"), 0700))
	must(t, os.Mkdir(filepath.Join(dir, ".hidden"), 0700))
	must(t, os.Symlink("project", filepath.Join(dir, "link")))
	must(t, os.WriteFile(filepath.Join(dir, "README"), nil, 0600))

	jar, err := cookiejar.New(nil)
	must(t, err)
	client := &http.Client{Jar: jar}
	defer client.CloseIdleConnections()
	ctx := testwait.Ctx(t)
	_, err = circusv1connect.NewAuthServiceClient(client, h.base).Login(ctx, connect.NewRequest(&circusv1.LoginRequest{
		Email: enrollmentEmail, Password: enrollmentPassword,
	}))
	must(t, err)
	api := circusv1connect.NewMachineServiceClient(client, h.base)
	res, err := api.ListDirectory(ctx, connect.NewRequest(&circusv1.ListDirectoryRequest{Device: "desk", Path: "~/browse"}))
	must(t, err)
	want := &circusv1.ListDirectoryResult{Path: dir, Parent: h.dir, Home: h.dir,
		Entries: []*circusv1.DirectoryEntry{
			{Name: "link", GitRepository: true, Symlink: true},
			{Name: "project", GitRepository: true},
		}}
	if !proto.Equal(res.Msg.GetResult(), want) {
		t.Fatalf("directory listing = %v, want %v", res.Msg.GetResult(), want)
	}

	res, err = api.ListDirectory(ctx, connect.NewRequest(&circusv1.ListDirectoryRequest{Device: "desk", Path: "~/browse/missing"}))
	must(t, err)
	if result := res.Msg.GetResult(); result.GetError() != circusv1.DirectoryError_DIRECTORY_ERROR_NOT_FOUND || result.GetParent() != dir {
		t.Fatalf("path-level error = %v", result)
	}
}
