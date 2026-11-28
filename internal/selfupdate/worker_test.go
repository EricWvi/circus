package selfupdate_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/EricWvi/circus/internal/selfupdate"
	"github.com/EricWvi/circus/internal/testutil/fakerelease"
)

func TestWorkerPackage(t *testing.T) {
	const version = "v1.2.3"
	srv := fakerelease.New(t)
	archive := srv.Publish(version, []string{"linux-amd64", "darwin-arm64"}, []fakerelease.File{{Name: "circus-worker", Body: "x", Mode: 0o755}})
	rel := srv.Release()
	p, err := rel.WorkerPackage(t.Context(), version, "darwin-arm64")
	if err != nil {
		t.Fatal(err)
	}
	name := "circus-worker-v1.2.3-darwin-arm64.tar.gz"
	if p.Version != version || p.Target != "darwin-arm64" || p.Name != name || p.URL != srv.Base()+"download/v1.2.3/"+name || p.SHA256 != sha256.Sum256(archive) {
		t.Errorf("package = %+v", p)
	}
	var got bytes.Buffer
	if err := rel.DownloadWorker(t.Context(), p, &got); err != nil || !bytes.Equal(got.Bytes(), archive) {
		t.Errorf("download = %d bytes, %v", got.Len(), err)
	}
	p.SHA256[0] ^= 1
	if err := rel.DownloadWorker(t.Context(), p, &got); !errors.Is(err, selfupdate.ErrDigestMismatch) {
		t.Errorf("download with another digest = %v", err)
	}

	asset := func(change func(map[string]string)) []byte {
		a := map[string]string{
			"target": "linux-amd64", "name": "circus-worker-v1.2.3-linux-amd64.tar.gz",
			"url":    srv.Base() + "download/v1.2.3/circus-worker-v1.2.3-linux-amd64.tar.gz",
			"sha256": hex.EncodeToString(make([]byte, 32)),
		}
		change(a)
		data, _ := json.Marshal(map[string]any{"version": version, "worker_assets": []any{a}})
		return data
	}
	for _, tc := range []struct {
		name, version, target string
		manifest              []byte
		wantErr               string
	}{
		{"development build", "dev", "linux-amd64", nil, `"dev" is not a release version`},
		{"unsupported platform", version, "windows-amd64", nil, "no circus-worker release for this platform (windows-amd64)"},
		{"missing target", version, "linux-arm64", nil, "no circus-worker release for this platform (linux-arm64)"},
		{"the CLI's assets only", version, "linux-amd64", []byte(`{"version":"v1.2.3","assets":[{"target":"linux-amd64"}]}`), "no circus-worker release"},
		{"another host", version, "linux-amd64", asset(func(a map[string]string) { a["url"] = "https://example.com/x.tar.gz" }), "invalid asset name or URL"},
		{"another name", version, "linux-amd64", asset(func(a map[string]string) { a["name"] = "circus-v1.2.3-linux-amd64.tar.gz" }), "invalid asset name or URL"},
		{"bad digest", version, "linux-amd64", asset(func(a map[string]string) { a["sha256"] = "zz" }), "invalid asset SHA-256"},
		{"not JSON", version, "linux-amd64", []byte("<html>"), "invalid latest.json of v1.2.3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.manifest != nil {
				srv.Set(fakerelease.Path(version, "latest.json"), tc.manifest)
			}
			if _, err := rel.WorkerPackage(t.Context(), tc.version, tc.target); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("WorkerPackage = %v, want %q", err, tc.wantErr)
			}
		})
	}

	http := rel
	http.BaseURL = strings.Replace(srv.Base(), "https://", "http://", 1)
	if _, err := http.WorkerPackage(t.Context(), version, "linux-amd64"); err == nil || !strings.Contains(err.Error(), "require HTTPS") {
		t.Errorf("plain HTTP = %v", err)
	}
}

func TestIsRelease(t *testing.T) {
	for v, want := range map[string]bool{"v1.2.3": true, "v1.0.0-rc.1": true, "dev": false, "": false, "1.2.3": false, "v1.2": false, "v1.2.3/../x": false} {
		if got := selfupdate.IsRelease(v); got != want {
			t.Errorf("IsRelease(%q) = %t", v, got)
		}
	}
}

func TestLatest(t *testing.T) {
	srv := fakerelease.New(t)
	rel := srv.Release()
	const path = "/releases/latest/download/latest.json"
	srv.Set(path, []byte(`{"version":"v1.4.0","assets":[]}`))
	if v, err := rel.Latest(t.Context()); err != nil || v != "v1.4.0" {
		t.Fatalf("Latest = %q, %v", v, err)
	}
	if got := srv.Requests(); len(got) != 1 || got[0] != path {
		t.Errorf("requests = %q", got)
	}
	for _, tc := range []struct{ name, body, wantErr string }{
		{"not JSON", "<html>", "invalid latest.json"},
		{"not a release", `{"version":"dev"}`, "invalid version in latest.json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv.Set(path, []byte(tc.body))
			if _, err := rel.Latest(t.Context()); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Latest = %v, want %q", err, tc.wantErr)
			}
		})
	}
	srv.Handle(path, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) })
	if _, err := rel.Latest(t.Context()); err == nil || !strings.Contains(err.Error(), "HTTP status 502") {
		t.Errorf("Latest on a server error = %v", err)
	}
}
