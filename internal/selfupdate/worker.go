package selfupdate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// WorkerArchiveLimit bounds a worker release package download.
const WorkerArchiveLimit = 256 << 20

// Release is where release manifests and archives come from: the GitHub
// releases of circus, or a test server laid out the same way.
type Release struct {
	// BaseURL ends in "/releases/": a release's files are at
	// BaseURL + "download/<tag>/<name>".
	BaseURL string
	// Client makes the requests; only HTTPS URLs are fetched.
	Client *http.Client
	// Current is this circus's version, sent in the User-Agent.
	Current string
}

// DefaultRelease reads the circus releases on GitHub.
func DefaultRelease(current string) Release {
	return Release{BaseURL: releasesURL, Client: releaseClient(), Current: current}
}

// WorkerPackage is the worker's release package for one platform, as its
// release's manifest lists it.
type WorkerPackage struct {
	Version string
	Target  string
	Name    string
	URL     string
	SHA256  [sha256.Size]byte
}

// WorkerPackage reads the manifest (latest.json) of release version and
// returns its worker package for target (such as "linux-amd64").
func (r Release) WorkerPackage(ctx context.Context, version, target string) (WorkerPackage, error) {
	if !IsRelease(version) {
		return WorkerPackage{}, fmt.Errorf("%q is not a release version", version)
	}
	switch target {
	case "linux-amd64", "linux-arm64", "darwin-amd64", "darwin-arm64":
	default:
		return WorkerPackage{}, fmt.Errorf("no circus-worker release for this platform (%s)", target)
	}
	var data bytes.Buffer
	if err := fetch(ctx, r.Client, r.Current, r.BaseURL+"download/"+version+"/latest.json", manifestLimit, &data); err != nil {
		return WorkerPackage{}, fmt.Errorf("download the %s release manifest: %w", version, err)
	}
	var m manifest
	if err := json.Unmarshal(data.Bytes(), &m); err != nil {
		return WorkerPackage{}, fmt.Errorf("invalid latest.json of %s: %w", version, err)
	}
	if m.Version != version {
		return WorkerPackage{}, fmt.Errorf("the latest.json of %s names version %q", version, m.Version)
	}
	a, digest, err := selectAsset(m.WorkerAssets, r.BaseURL, "circus-worker", version, target)
	if err != nil {
		return WorkerPackage{}, err
	}
	return WorkerPackage{Version: version, Target: target, Name: a.Name, URL: a.URL, SHA256: digest}, nil
}

// ErrDigestMismatch reports a download that is not the archive the
// manifest describes.
var ErrDigestMismatch = errors.New("SHA-256 mismatch")

// DownloadWorker writes p's archive to w and checks it against the
// manifest's digest. What w received is trusted only when it returns nil.
func (r Release) DownloadWorker(ctx context.Context, p WorkerPackage, w io.Writer) error {
	hash := sha256.New()
	if err := fetch(ctx, r.Client, r.Current, p.URL, WorkerArchiveLimit, io.MultiWriter(w, hash)); err != nil {
		return fmt.Errorf("download %s: %w", p.Name, err)
	}
	if [sha256.Size]byte(hash.Sum(nil)) != p.SHA256 {
		return fmt.Errorf("%w for %s", ErrDigestMismatch, p.Name)
	}
	return nil
}
