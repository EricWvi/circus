package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

const (
	testVersion = "v1.2.3"
	testTarget  = "linux-amd64"
	oldBinary   = "current CLI"
	newBinary   = "downloaded CLI"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

type unreadBody struct{ t *testing.T }

func (b unreadBody) Read([]byte) (int, error) {
	b.t.Fatal("download body was read before validating status and length")
	return 0, io.EOF
}

func (unreadBody) Close() error { return nil }

func testArchive(t *testing.T, name string, kind byte, contents string, duplicate bool) []byte {
	t.Helper()
	var data bytes.Buffer
	gz := gzip.NewWriter(&data)
	tw := tar.NewWriter(gz)
	for count := 0; count == 0 || duplicate && count == 1; count++ {
		h := &tar.Header{Name: name, Typeflag: kind, Size: int64(len(contents)), Mode: 0o755}
		if kind == tar.TypeSymlink {
			h.Linkname = "elsewhere"
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tw, contents); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func TestUpdate(t *testing.T) {
	const assetName = "circus-" + testVersion + "-" + testTarget + ".tar.gz"
	const assetURL = releasesURL + "download/" + testVersion + "/" + assetName
	for _, tc := range []struct {
		name         string
		variant      string
		wantErr      string
		wantRequests []string
	}{
		{name: "newer release installs", wantRequests: []string{manifestURL, assetURL}},
		{name: "already current", variant: "current", wantRequests: []string{manifestURL}},
		{name: "older release refused", variant: "older", wantErr: "latest release v1.2.3 is older than this circus (v1.10.0); pass --force to install it", wantRequests: []string{manifestURL}},
		{name: "older release with force installs", variant: "force", wantRequests: []string{manifestURL, assetURL}},
		{name: "dev replaced", variant: "dev", wantRequests: []string{manifestURL, assetURL}},
		{name: "unsupported platform", variant: "platform", wantErr: "no circus release for this platform"},
		{name: "bad version", variant: "version", wantErr: "invalid version", wantRequests: []string{manifestURL}},
		{name: "incomplete version", variant: "incomplete version", wantErr: "invalid version", wantRequests: []string{manifestURL}},
		{name: "wrong name", variant: "name", wantErr: "invalid asset name", wantRequests: []string{manifestURL}},
		{name: "wrong URL", variant: "url", wantErr: "invalid asset name or URL", wantRequests: []string{manifestURL}},
		{name: "duplicate asset", variant: "duplicate", wantErr: "duplicate asset", wantRequests: []string{manifestURL}},
		{name: "bad checksum format", variant: "bad hash", wantErr: "invalid asset SHA-256", wantRequests: []string{manifestURL}},
		{name: "66-character checksum", variant: "long hash", wantErr: "invalid asset SHA-256", wantRequests: []string{manifestURL}},
		{name: "checksum mismatch", variant: "mismatch", wantErr: "SHA-256 mismatch", wantRequests: []string{manifestURL, assetURL}},
		{name: "oversized download", variant: "oversized", wantErr: "exceeds size limit", wantRequests: []string{manifestURL, assetURL}},
		{name: "archive without circus", variant: "missing", wantErr: "regular file named circus", wantRequests: []string{manifestURL, assetURL}},
		{name: "archive with symlink", variant: "symlink", wantErr: "regular file named circus", wantRequests: []string{manifestURL, assetURL}},
		{name: "archive with two files", variant: "two files", wantErr: "regular file named circus", wantRequests: []string{manifestURL, assetURL}},
		{name: "HTTP error status", variant: "HTTP error", wantErr: "HTTP status 404", wantRequests: []string{manifestURL}},
		{name: "cancelled before rename", variant: "cancel", wantErr: "context canceled", wantRequests: []string{manifestURL, assetURL}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "circus")
			const mode = 0o750
			if err := os.WriteFile(bin, []byte(oldBinary), mode); err != nil {
				t.Fatal(err)
			}
			exe := bin
			if runtime.GOOS != "windows" {
				exe = filepath.Join(dir, "link")
				if err := os.Symlink(bin, exe); err != nil {
					t.Fatal(err)
				}
			}

			archive := testArchive(t, "circus", tar.TypeReg, newBinary, false)
			switch tc.variant {
			case "missing":
				archive = testArchive(t, "other", tar.TypeReg, newBinary, false)
			case "symlink":
				archive = testArchive(t, "circus", tar.TypeSymlink, "", false)
			case "two files":
				archive = testArchive(t, "circus", tar.TypeReg, newBinary, true)
			}
			m := manifest{Version: testVersion, Assets: []asset{{
				Target: testTarget, Name: assetName, URL: assetURL, SHA256: fmt.Sprintf("%x", sha256.Sum256(archive)),
			}}}
			current, target := "v1.2.3-rc.1", testTarget
			switch tc.variant {
			case "current":
				current = testVersion
			case "older", "force":
				current = "v1.10.0"
			case "dev":
				current = "dev"
			case "platform":
				target = "windows-amd64"
			case "version":
				m.Version = "v../other"
			case "incomplete version":
				m.Version = "v1.2"
			case "name":
				m.Assets[0].Name = "other.tar.gz"
			case "url":
				m.Assets[0].URL = releasesURL + "download/v0.0.1/" + assetName
			case "duplicate":
				m.Assets = append(m.Assets, m.Assets[0])
			case "bad hash":
				m.Assets[0].SHA256 = strings.Repeat("z", 64)
			case "long hash":
				m.Assets[0].SHA256 += "00"
			case "mismatch":
				archive[0] ^= 1
			}
			data, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			var requests []string
			client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				requests = append(requests, req.URL.String())
				if got := req.Header.Get("User-Agent"); got != "circus/"+current {
					t.Errorf("User-Agent = %q", got)
				}
				switch req.URL.String() {
				case manifestURL:
					if tc.variant == "HTTP error" {
						return &http.Response{StatusCode: http.StatusNotFound, Body: unreadBody{t}}, nil
					}
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(data))}, nil
				case assetURL:
					if tc.variant == "oversized" {
						return &http.Response{StatusCode: http.StatusOK, ContentLength: archiveLimit + 1, Body: unreadBody{t}}, nil
					}
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(archive))}, nil
				default:
					t.Fatalf("unexpected URL %s", req.URL)
					return nil, nil
				}
			})}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var notice bytes.Buffer
			result, err := update(ctx, httpDownloader(client, current), current, target, tc.variant == "force", &notice, func() (string, error) {
				if tc.variant == "current" {
					t.Fatal("already current but still resolving the executable")
				}
				if tc.variant == "cancel" {
					cancel()
				}
				if tc.variant == "dev" && notice.String() != "Replacing dev with "+testVersion+"\n" {
					t.Errorf("notice before installation = %q", notice.String())
				}
				return exe, nil
			})

			wantBinary := oldBinary
			switch {
			case tc.wantErr != "":
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error = %v, want %q", err, tc.wantErr)
				}
				if result.ReplacedPath != "" {
					t.Errorf("failed update reported a replacement: %+v", result)
				}
				if tc.variant == "cancel" && !errors.Is(err, context.Canceled) {
					t.Errorf("error = %v, want context.Canceled", err)
				}
			case tc.variant == "current":
				if err != nil || result.Message != "circus is already at "+testVersion || result.ReplacedPath != "" {
					t.Errorf("update = %+v, %v", result, err)
				}
			default:
				resolved, resolveErr := filepath.EvalSymlinks(bin)
				if resolveErr != nil {
					t.Fatal(resolveErr)
				}
				if err != nil || result.Message != "Updated circus to "+testVersion || result.ReplacedPath != resolved || result.Version != testVersion {
					t.Errorf("update = %+v, %v", result, err)
				}
				wantBinary = newBinary
			}
			if !slices.Equal(requests, tc.wantRequests) {
				t.Errorf("requested %v, want %v", requests, tc.wantRequests)
			}
			if got, err := os.ReadFile(bin); err != nil || string(got) != wantBinary {
				t.Errorf("binary = %q, %v, want %q", got, err, wantBinary)
			}
			if runtime.GOOS != "windows" {
				if info, err := os.Stat(bin); err != nil || info.Mode().Perm() != mode {
					t.Errorf("binary mode: %v, %v, want %o", info, err, mode)
				}
				if info, err := os.Lstat(exe); err != nil || info.Mode()&os.ModeSymlink == 0 {
					t.Errorf("executable link changed: %v, %v", info, err)
				}
			}
			if staged, err := filepath.Glob(filepath.Join(dir, ".circus-update-*")); err != nil || len(staged) != 0 {
				t.Errorf("staging files left behind: %v, %v", staged, err)
			}
		})
	}
}

func TestDownloadBoundsAndRedirects(t *testing.T) {
	for _, tc := range []struct {
		name, url, redirect, wantErr string
		wantRequests                 int
	}{
		{"unknown length", manifestURL, "", "exceeds size limit", 1},
		{"HTTP manifest URL", "http://example.com/latest.json", "", "require HTTPS", 0},
		{"HTTPS redirect required", manifestURL, "http://example.com/", "require HTTPS", 1},
		{"redirect limit", manifestURL, "https://example.com/", "too many release download redirects", 11},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := releaseClient()
			requests := 0
			client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
				requests++
				if tc.redirect != "" {
					return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": {tc.redirect}}, Body: http.NoBody}, nil
				}
				return &http.Response{StatusCode: http.StatusOK, ContentLength: -1, Body: io.NopCloser(strings.NewReader("oversized"))}, nil
			})
			_, err := httpDownloader(client, "dev")(t.Context(), tc.url, 4)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) || requests != tc.wantRequests {
				t.Errorf("download: %v after %d requests, want %q after %d requests", err, requests, tc.wantErr, tc.wantRequests)
			}
		})
	}
}

type readFunc func([]byte) (int, error)

func (f readFunc) Read(p []byte) (int, error) { return f(p) }

func TestDownloadIdleTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			progress := false
			body := readFunc(func(p []byte) (int, error) {
				if !progress {
					time.Sleep(readIdleLimit / 2)
					progress = true
					p[0] = 'x'
					return 1, nil
				}
				<-req.Context().Done()
				return 0, req.Context().Err()
			})
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(body)}, nil
		})}
		_, err := httpDownloader(client, "dev")(t.Context(), manifestURL, 4)
		if err == nil || !strings.Contains(err.Error(), "release download stalled") {
			t.Fatalf("download error = %v", err)
		}
		if elapsed := time.Since(start); elapsed != readIdleLimit*3/2 {
			t.Errorf("idle timeout elapsed = %s, want %s", elapsed, readIdleLimit*3/2)
		}
	})
}

func TestExtract(t *testing.T) {
	valid := testArchive(t, "circus", tar.TypeReg, newBinary, false)
	makeGzip := func(write func(io.Writer) error) []byte {
		t.Helper()
		var data bytes.Buffer
		gz, err := gzip.NewWriterLevel(&data, gzip.BestSpeed)
		if err != nil {
			t.Fatal(err)
		}
		if err := write(gz); err != nil {
			t.Fatal(err)
		}
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
		return data.Bytes()
	}
	emptyMember := makeGzip(func(io.Writer) error { return nil })
	oversizedHeader := makeGzip(func(w io.Writer) error {
		tw := tar.NewWriter(w)
		// A header alone is enough to detect the oversized claim.
		return tw.WriteHeader(&tar.Header{Name: "circus", Mode: 0o755, Size: archiveLimit + 1})
	})
	bomb := makeGzip(func(w io.Writer) error {
		tw := tar.NewWriter(w)
		if err := tw.WriteHeader(&tar.Header{Name: "circus", Mode: 0o755, Size: 1}); err != nil {
			return err
		}
		if _, err := io.WriteString(tw, "x"); err != nil {
			return err
		}
		if err := tw.Close(); err != nil {
			return err
		}
		zeros := make([]byte, 1<<20)
		for range (archiveLimit >> 20) + 2 {
			if _, err := w.Write(zeros); err != nil {
				return err
			}
		}
		return nil
	})
	for _, tc := range []struct {
		name    string
		archive []byte
	}{
		{"trailing gzip garbage", append(bytes.Clone(valid), "garbage"...)},
		{"concatenated gzip members", append(bytes.Clone(valid), emptyMember...)},
		{"zero-length circus", testArchive(t, "circus", tar.TypeReg, "", false)},
		{"oversized header", oversizedHeader},
		{"decompression bomb", bomb},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := extract(tc.archive, io.Discard); err == nil || !strings.Contains(err.Error(), "within the size limit") {
				t.Errorf("extract error = %v, want invalid archive", err)
			}
		})
	}
}

// Check downloads and verifies but changes nothing; only Apply replaces the
// executable. `circus update` relies on it to abort with the old binary in
// place when the worker package can't be had.
func TestCheckChangesNothingUntilApply(t *testing.T) {
	const assetName = "circus-" + testVersion + "-" + testTarget + ".tar.gz"
	const assetURL = releasesURL + "download/" + testVersion + "/" + assetName
	dir := t.TempDir()
	bin := filepath.Join(dir, "circus")
	if err := os.WriteFile(bin, []byte(oldBinary), 0o755); err != nil {
		t.Fatal(err)
	}
	archive := testArchive(t, "circus", tar.TypeReg, newBinary, false)
	data, err := json.Marshal(manifest{Version: testVersion, Assets: []asset{{
		Target: testTarget, Name: assetName, URL: assetURL, SHA256: fmt.Sprintf("%x", sha256.Sum256(archive)),
	}}})
	if err != nil {
		t.Fatal(err)
	}
	var requests []string
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests = append(requests, req.URL.String())
		body := data
		if req.URL.String() == assetURL {
			body = archive
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body))}, nil
	})}
	plan, err := check(t.Context(), httpDownloader(client, "v1.0.0"), "v1.0.0", testTarget, false, io.Discard, func() (string, error) { return bin, nil })
	if err != nil || plan.UpToDate || plan.Version != testVersion || plan.Path != bin || plan.Apply == nil {
		t.Fatalf("check = %+v, %v", plan, err)
	}
	if !slices.Equal(requests, []string{manifestURL, assetURL}) {
		t.Errorf("check requested %v", requests)
	}
	if got, err := os.ReadFile(bin); err != nil || string(got) != oldBinary {
		t.Fatalf("check changed the executable: %q, %v", got, err)
	}
	if staged, _ := filepath.Glob(filepath.Join(dir, ".circus-update-*")); len(staged) != 0 {
		t.Errorf("check staged %v", staged)
	}
	result, err := plan.Apply(t.Context())
	if err != nil || result.ReplacedPath != bin || result.Version != testVersion {
		t.Fatalf("apply = %+v, %v", result, err)
	}
	if got, err := os.ReadFile(bin); err != nil || string(got) != newBinary {
		t.Errorf("apply left %q, %v", got, err)
	}
	if len(requests) != 2 {
		t.Errorf("apply downloaded again: %v", requests)
	}
}
