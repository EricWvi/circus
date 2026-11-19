// Package selfupdate verifies and installs the latest circus CLI release.
package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"syscall"
	"time"

	"golang.org/x/mod/semver"
)

const (
	releasesURL   = "https://github.com/EricWvi/circus/releases/"
	manifestURL   = releasesURL + "latest/download/latest.json"
	manifestLimit = 1 << 20
	archiveLimit  = 128 << 20
	readIdleLimit = 60 * time.Second
)

var versionPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`)

type manifest struct {
	Version string  `json:"version"`
	Assets  []asset `json:"assets"`
}

type asset struct {
	Target string `json:"target"`
	Name   string `json:"name"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

type downloader func(context.Context, string, int64) ([]byte, error)

// Run checks for the latest release and replaces the running executable if needed.
func Run(ctx context.Context, current string, force bool, out io.Writer) (string, error) {
	client := releaseClient()
	defer client.CloseIdleConnections()
	return update(ctx, httpDownloader(client, current), current, runtime.GOOS+"-"+runtime.GOARCH, force, out, os.Executable)
}

func releaseClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	transport.TLSHandshakeTimeout = 15 * time.Second
	transport.ResponseHeaderTimeout = 30 * time.Second
	transport.Proxy = http.ProxyFromEnvironment
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if req.URL.Scheme != "https" {
				return errors.New("release downloads require HTTPS")
			}
			if len(via) > 10 {
				return errors.New("too many release download redirects")
			}
			return nil
		},
	}
}

func httpDownloader(client *http.Client, current string) downloader {
	return func(ctx context.Context, url string, limit int64) ([]byte, error) {
		ctx, cancel := context.WithCancelCause(ctx)
		defer cancel(nil)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		if req.URL.Scheme != "https" {
			return nil, errors.New("release downloads require HTTPS")
		}
		req.Header.Set("User-Agent", "circus/"+current)
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("HTTP status %d", resp.StatusCode)
		}
		if resp.ContentLength > limit {
			return nil, errors.New("download exceeds size limit")
		}
		timer := time.AfterFunc(readIdleLimit, func() {
			cancel(errors.New("release download stalled: no bytes received for 60 seconds"))
		})
		defer timer.Stop()
		data, err := io.ReadAll(io.LimitReader(idleReader{resp.Body, timer}, limit+1))
		if cause := context.Cause(ctx); cause != nil {
			return nil, cause
		}
		if err != nil {
			return nil, err
		}
		if int64(len(data)) > limit {
			return nil, errors.New("download exceeds size limit")
		}
		return data, nil
	}
}

// idleReader extends the deadline only when the body makes progress.
type idleReader struct {
	body  io.Reader
	timer *time.Timer
}

func (r idleReader) Read(p []byte) (int, error) {
	n, err := r.body.Read(p)
	if n > 0 {
		r.timer.Reset(readIdleLimit)
	}
	return n, err
}

func update(ctx context.Context, download downloader, current, target string, force bool, out io.Writer, executable func() (string, error)) (string, error) {
	switch target {
	case "linux-amd64", "linux-arm64", "darwin-amd64", "darwin-arm64":
	default:
		return "", fmt.Errorf("no circus release for this platform (%s)", target)
	}
	data, err := download(ctx, manifestURL, manifestLimit)
	if err != nil {
		return "", fmt.Errorf("download latest.json: %w", err)
	}
	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return "", fmt.Errorf("invalid latest.json: %w", err)
	}
	a, digest, err := selectAsset(m, target)
	if err != nil {
		return "", err
	}
	currentValid := semver.IsValid(current)
	if currentValid {
		switch semver.Compare(m.Version, current) {
		case 0:
			return "circus is already at " + current, nil
		case -1:
			if !force {
				return "", fmt.Errorf("latest release %s is older than this circus (%s); pass --force to install it", m.Version, current)
			}
		}
	}
	archive, err := download(ctx, a.URL, archiveLimit)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", a.Name, err)
	}
	if sha256.Sum256(archive) != digest {
		return "", fmt.Errorf("SHA-256 mismatch for %s", a.Name)
	}
	if !currentValid {
		if _, err := fmt.Fprintf(out, "Replacing %s with %s\n", current, m.Version); err != nil {
			return "", err
		}
	}
	if err := install(ctx, executable, archive); err != nil {
		return "", err
	}
	return "Updated circus to " + m.Version, nil
}

func selectAsset(m manifest, target string) (asset, [sha256.Size]byte, error) {
	var digest [sha256.Size]byte
	if !versionPattern.MatchString(m.Version) || !semver.IsValid(m.Version) {
		return asset{}, digest, errors.New("invalid version in latest.json")
	}
	var selected *asset
	for _, a := range m.Assets {
		if a.Target != target {
			continue
		}
		if selected != nil {
			return asset{}, digest, errors.New("duplicate asset for this platform in latest.json")
		}
		selected = &a
	}
	if selected == nil {
		return asset{}, digest, fmt.Errorf("no circus release for this platform (%s)", target)
	}
	name := "circus-" + m.Version + "-" + target + ".tar.gz"
	url := releasesURL + "download/" + m.Version + "/" + name
	if selected.Name != name || selected.URL != url {
		return asset{}, digest, errors.New("invalid asset name or URL in latest.json")
	}
	if len(selected.SHA256) != hex.EncodedLen(len(digest)) {
		return asset{}, digest, errors.New("invalid asset SHA-256 in latest.json")
	}
	if _, err := hex.Decode(digest[:], []byte(selected.SHA256)); err != nil {
		return asset{}, digest, errors.New("invalid asset SHA-256 in latest.json")
	}
	return *selected, digest, nil
}

func install(ctx context.Context, executable func() (string, error), archive []byte) error {
	path, err := executable()
	if err != nil {
		return fmt.Errorf("locate current executable: %w", err)
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("resolve current executable: %w", err)
	}
	mode := os.FileMode(0o755)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	staged, err := os.CreateTemp(filepath.Dir(path), ".circus-update-*")
	if err != nil {
		return installationError(err)
	}
	defer os.Remove(staged.Name())
	defer staged.Close()
	if err := extract(archive, staged); err != nil {
		return fmt.Errorf("prepare release archive: %w", err)
	}
	if err := staged.Chmod(mode); err != nil {
		return installationError(err)
	}
	if err := staged.Sync(); err != nil {
		return installationError(err)
	}
	if err := staged.Close(); err != nil {
		return installationError(err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(staged.Name(), path); err != nil {
		return installationError(err)
	}
	return nil
}

func installationError(err error) error {
	if errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.EROFS) {
		return fmt.Errorf("cannot replace circus (check installation directory permissions): %w", err)
	}
	return fmt.Errorf("cannot replace circus: %w", err)
}

func extract(archive []byte, dst io.Writer) error {
	src := bytes.NewReader(archive)
	gz, err := gzip.NewReader(src)
	if err != nil {
		return err
	}
	defer gz.Close()
	gz.Multistream(false)
	// Allow room for tar headers and padding while bounding decompression work.
	expanded := &io.LimitedReader{R: gz, N: archiveLimit + (1 << 20) + 1}
	tarball := tar.NewReader(expanded)
	header, err := tarball.Next()
	if err != nil && err != io.EOF {
		return err
	}
	invalid := errors.New("archive must contain only one regular file named circus within the size limit")
	if header == nil || header.Name != "circus" || header.Typeflag != tar.TypeReg || header.Size <= 0 || header.Size > archiveLimit {
		return invalid
	}
	if _, err := io.Copy(dst, tarball); err != nil {
		return err
	}
	if _, err := tarball.Next(); err != io.EOF {
		if err != nil {
			return err
		}
		return invalid
	}
	// Read through the gzip trailer to check its integrity.
	if _, err := io.Copy(io.Discard, expanded); err != nil {
		return err
	}
	if expanded.N == 0 || src.Len() != 0 {
		return invalid
	}
	return nil
}
