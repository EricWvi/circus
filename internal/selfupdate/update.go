// Package selfupdate verifies and installs the latest circus CLI release,
// and reads and downloads the worker release packages of a release.
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
	// WorkerAssets are the worker's release packages, one per target.
	WorkerAssets []asset `json:"worker_assets"`
}

type asset struct {
	Target string `json:"target"`
	Name   string `json:"name"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

type downloader func(context.Context, string, int64) ([]byte, error)

// Result reports the update outcome and the executable path replaced, if any.
type Result struct {
	Message      string
	ReplacedPath string
	// Version is the release installed at ReplacedPath.
	Version string
}

// IsRelease reports whether version is a release tag (vX.Y.Z with an
// optional pre-release); anything else, such as "dev", has no release.
func IsRelease(version string) bool {
	return versionPattern.MatchString(version) && semver.IsValid(version)
}

// Plan is a checked update. For an update, the latest release's archive
// is downloaded and verified and the executable to replace is known, but
// nothing has changed yet: Apply replaces it.
type Plan struct {
	// Version is the latest release.
	Version string
	// UpToDate is true when this circus already is the latest release;
	// Message says so, and there is nothing to apply.
	UpToDate bool
	Message  string
	// Path is the executable Apply replaces, with symlinks resolved.
	Path string
	// Apply installs the release at Path. It is nil when UpToDate.
	Apply func(context.Context) (Result, error)
}

// Check finds the latest release and, unless this circus already is it,
// downloads and verifies its archive for this platform.
func Check(ctx context.Context, current string, force bool, out io.Writer) (Plan, error) {
	client := releaseClient()
	defer client.CloseIdleConnections()
	return check(ctx, httpDownloader(client, current), current, runtime.GOOS+"-"+runtime.GOARCH, force, out, os.Executable)
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
		var data bytes.Buffer
		if err := fetch(ctx, client, current, url, limit, &data); err != nil {
			return nil, err
		}
		return data.Bytes(), nil
	}
}

// fetch copies the body at url to w: at most limit bytes over HTTPS,
// failing when no bytes arrive for readIdleLimit.
func fetch(ctx context.Context, client *http.Client, current, url string, limit int64, w io.Writer) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if req.URL.Scheme != "https" {
		return errors.New("release downloads require HTTPS")
	}
	req.Header.Set("User-Agent", "circus/"+current)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP status %d", resp.StatusCode)
	}
	if resp.ContentLength > limit {
		return errors.New("download exceeds size limit")
	}
	timer := time.AfterFunc(readIdleLimit, func() {
		cancel(errors.New("release download stalled: no bytes received for 60 seconds"))
	})
	defer timer.Stop()
	n, err := io.Copy(w, io.LimitReader(idleReader{resp.Body, timer}, limit+1))
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	if err != nil {
		return err
	}
	if n > limit {
		return errors.New("download exceeds size limit")
	}
	return nil
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

// update is Check followed by Apply.
func update(ctx context.Context, download downloader, current, target string, force bool, out io.Writer, executable func() (string, error)) (Result, error) {
	p, err := check(ctx, download, current, target, force, out, executable)
	if err != nil || p.UpToDate {
		return Result{Message: p.Message}, err
	}
	return p.Apply(ctx)
}

func check(ctx context.Context, download downloader, current, target string, force bool, out io.Writer, executable func() (string, error)) (Plan, error) {
	switch target {
	case "linux-amd64", "linux-arm64", "darwin-amd64", "darwin-arm64":
	default:
		return Plan{}, fmt.Errorf("no circus release for this platform (%s)", target)
	}
	data, err := download(ctx, manifestURL, manifestLimit)
	if err != nil {
		return Plan{}, fmt.Errorf("download latest.json: %w", err)
	}
	m, err := parseLatest(data)
	if err != nil {
		return Plan{}, err
	}
	a, digest, err := selectAsset(m.Assets, releasesURL, "circus", m.Version, target)
	if err != nil {
		return Plan{}, err
	}
	currentValid := semver.IsValid(current)
	if currentValid {
		switch semver.Compare(m.Version, current) {
		case 0:
			return Plan{Version: m.Version, UpToDate: true, Message: "circus is already at " + current}, nil
		case -1:
			if !force {
				return Plan{}, fmt.Errorf("latest release %s is older than this circus (%s); pass --force to install it", m.Version, current)
			}
		}
	}
	archive, err := download(ctx, a.URL, archiveLimit)
	if err != nil {
		return Plan{}, fmt.Errorf("download %s: %w", a.Name, err)
	}
	if sha256.Sum256(archive) != digest {
		return Plan{}, fmt.Errorf("SHA-256 mismatch for %s", a.Name)
	}
	if !currentValid {
		if _, err := fmt.Fprintf(out, "Replacing %s with %s\n", current, m.Version); err != nil {
			return Plan{}, err
		}
	}
	path, err := executable()
	if err != nil {
		return Plan{}, fmt.Errorf("locate current executable: %w", err)
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return Plan{}, fmt.Errorf("resolve current executable: %w", err)
	}
	version := m.Version
	return Plan{Version: version, Path: path, Apply: func(ctx context.Context) (Result, error) {
		if err := install(ctx, path, archive); err != nil {
			return Result{}, err
		}
		return Result{Message: "Updated circus to " + version, ReplacedPath: path, Version: version}, nil
	}}, nil
}

// parseLatest reads the latest release's manifest (latest.json).
func parseLatest(data []byte) (manifest, error) {
	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return manifest{}, fmt.Errorf("invalid latest.json: %w", err)
	}
	if !IsRelease(m.Version) {
		return manifest{}, errors.New("invalid version in latest.json")
	}
	return m, nil
}

// selectAsset picks product's archive for target from assets, checking its
// name and URL under base and its digest's format.
func selectAsset(assets []asset, base, product, version, target string) (asset, [sha256.Size]byte, error) {
	var digest [sha256.Size]byte
	var selected *asset
	for _, a := range assets {
		if a.Target != target {
			continue
		}
		if selected != nil {
			return asset{}, digest, errors.New("duplicate asset for this platform in latest.json")
		}
		selected = &a
	}
	if selected == nil {
		return asset{}, digest, fmt.Errorf("no %s release for this platform (%s)", product, target)
	}
	name := product + "-" + version + "-" + target + ".tar.gz"
	url := base + "download/" + version + "/" + name
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

// install replaces the executable at path, with symlinks resolved, by the
// one in archive.
func install(ctx context.Context, path string, archive []byte) error {
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
