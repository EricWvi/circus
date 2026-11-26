// Package workerservice installs the circus worker as a per-user service:
// it downloads the worker release package that matches this circus,
// verifies it against the release manifest, unpacks it under the worker's
// data directory and runs its launcher with userservice.Worker.
//
// Every operation that reads or changes the worker's service file or its
// package store takes the store's lock first and reads the service file
// under it. The store is always the one of the current data root (Root);
// a service file that runs something else is never used to find one.
package workerservice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/EricWvi/circus/internal/circushome"
	"github.com/EricWvi/circus/internal/credfile"
	"github.com/EricWvi/circus/internal/selfupdate"
	"github.com/EricWvi/circus/internal/userservice"
)

// Settings are the worker's own variables a service records when they are
// set at install time, besides what every circus service records. Node.js
// reads its TLS settings from NODE_EXTRA_CA_CERTS and NODE_USE_SYSTEM_CA,
// not from SSL_CERT_FILE or SSL_CERT_DIR.
var Settings = []string{
	"CIRCUS_WORKER_MAX_SESSIONS", "CIRCUS_WORKER_LOG_LEVEL", "CIRCUS_WORKER_IDLE_SECONDS",
	"CIRCUS_NPM_REGISTRY", "CLAUDE_CONFIG_DIR", "NODE_EXTRA_CA_CERTS", "NODE_USE_SYSTEM_CA",
}

// ProxyVariables are the proxy settings a service file records verbatim.
var ProxyVariables = []string{
	"HTTPS_PROXY", "HTTP_PROXY", "ALL_PROXY", "NO_PROXY",
	"https_proxy", "http_proxy", "all_proxy", "no_proxy",
}

// Options say where packages come from and what the service runs.
type Options struct {
	// Release serves the release manifests and packages.
	Release selfupdate.Release
	// Circus is the circus executable the worker runs `circus internal
	// exec-guard` from (CIRCUS_BIN).
	Circus string
	// Out receives progress and notices.
	Out io.Writer
}

// target is this machine's release platform; tests replace it.
var target = runtime.GOOS + "-" + runtime.GOARCH

// ErrDevelopmentBuild refuses an install from a circus without a release.
var ErrDevelopmentBuild = errors.New("this circus is a development build, so there is no worker release package to install. Run the worker from a circus checkout instead: `npm ci`, `task worker:build`, then `node worker/dist/main.js` (see worker/README.md)")

// Install installs the worker package of version (this circus's own),
// unless it is already there and intact, and installs and starts the worker
// service running it with the current shell's settings. Once the service
// runs the new package, other versions are removed.
func Install(ctx context.Context, version string, opts Options) (Node, error) {
	if !selfupdate.IsRelease(version) {
		return Node{}, fmt.Errorf("%w (version %q)", ErrDevelopmentBuild, version)
	}
	// Fails on a platform without a service manager before anything else.
	if _, _, err := userservice.Worker.Installed(); err != nil {
		return Node{}, err
	}
	credentials, err := credfile.Path(credfile.RoleWorker)
	if err != nil {
		return Node{}, err
	}
	if err := checkCredential(credentials); err != nil {
		return Node{}, fmt.Errorf("%w: enroll this machine as a worker first with `circus join --role worker` and a link for a worker", err)
	}
	node, err := FindNode(ctx)
	if err != nil {
		return Node{}, err
	}
	s, unlock, err := lockStore(ctx, opts.Out)
	if err != nil {
		return Node{}, err
	}
	defer unlock()
	if err := s.ensure(ctx, opts.Release, version, target, opts.Out); err != nil {
		return Node{}, err
	}
	env, skipped, err := userservice.Environment(Settings...)
	if err != nil {
		return Node{}, err
	}
	userservice.WarnSkipped(opts.Out, skipped)
	stop, err := prepareStopFile()
	if err != nil {
		return Node{}, err
	}
	env = withPrograms(env, node.Path, opts.Circus, stop)
	if err := userservice.Worker.Install(s.launcher(version), nil, env, opts.Out); err != nil {
		return Node{}, err
	}
	if err := s.prune(version); err != nil {
		fmt.Fprintf(opts.Out, "Warning: could not remove other worker versions from %s: %v\n", s.root, err)
	}
	return node, nil
}

// lockStore locks the store of the current data root.
func lockStore(ctx context.Context, out io.Writer) (store, func(), error) {
	root, err := Root()
	if err != nil {
		return store{}, nil, err
	}
	s := store{root: root}
	unlock, err := s.lock(ctx, out)
	return s, unlock, err
}

// checkCredential checks the worker credential file at path.
func checkCredential(path string) error {
	file, err := credfile.Load(path)
	if errors.Is(err, credfile.ErrNotFound) {
		return fmt.Errorf("no worker credential at %s", path)
	}
	if err != nil {
		return fmt.Errorf("the worker credential at %s is unusable: %w", path, err)
	}
	if file.Role != credfile.RoleWorker {
		return fmt.Errorf("%s holds %s credentials", path, file.Role)
	}
	return nil
}

// withPrograms sets CIRCUS_NODE, CIRCUS_BIN and the stop file in env,
// keeping it sorted.
func withPrograms(env []string, node, circus, stop string) []string {
	env = slices.DeleteFunc(env, func(entry string) bool {
		return strings.HasPrefix(entry, "CIRCUS_NODE=") || strings.HasPrefix(entry, "CIRCUS_BIN=") ||
			strings.HasPrefix(entry, StopFileEnv+"=")
	})
	env = append(env, "CIRCUS_NODE="+node, "CIRCUS_BIN="+circus, StopFileEnv+"="+stop)
	slices.Sort(env)
	return env
}

// Installed describes the installed worker service's package.
type Installed struct {
	// Version is the package version the service runs, "" when the service
	// file runs something else.
	Version string
	// Root is the directory the service's package is in, by its path.
	Root string
	// Store is the package store of the environment the service file
	// records (RootFor): the only directory circus changes for it.
	Store string
	// Node and Circus are the recorded CIRCUS_NODE and CIRCUS_BIN.
	Node, Circus string
	// Definition is the whole service file.
	Definition userservice.Definition
}

// InstalledService reads the worker's service file. ok is false when the
// service is not installed. Callers that act on it take the lock first
// (Prepare and Uninstall do).
func InstalledService() (Installed, bool, error) {
	d, ok, err := userservice.Worker.Installed()
	if !ok || err != nil {
		return Installed{}, ok, err
	}
	store, err := RootFor(d.Env)
	if err != nil {
		return Installed{}, true, err
	}
	i := Installed{Store: store, Node: d.Env["CIRCUS_NODE"], Circus: d.Env["CIRCUS_BIN"], Definition: d}
	launcher := d.Program()
	if version := filepath.Base(filepath.Dir(launcher)); filepath.Base(launcher) == launcherName && selfupdate.IsRelease(version) {
		i.Version, i.Root = version, filepath.Dir(filepath.Dir(launcher))
	}
	return i, true, nil
}

// owns refuses a service file that runs anything but a package of the
// store its own environment names.
func (i Installed) owns() error {
	if i.Version != "" && filepath.Clean(i.Root) == filepath.Clean(i.Store) {
		return nil
	}
	return fmt.Errorf("the circus worker service runs %q, which is not a worker package in %s, the worker packages of the CIRCUS_HOME, XDG_DATA_HOME and HOME its service file records. "+
		"Reinstall it with `circus worker install`; circus does not change other directories", i.Definition.Program(), i.Store)
}

// lockInstalled locks the store of the installed service (the one its
// service file's environment names), or of this process's environment
// when none is installed, and reads the service file again under the lock.
func lockInstalled(ctx context.Context, out io.Writer) (store, func(), Installed, bool, error) {
	for attempt := 0; ; attempt++ {
		before, ok, err := InstalledService()
		if err != nil {
			return store{}, nil, Installed{}, false, err
		}
		root := before.Store
		if !ok {
			if root, err = Root(); err != nil {
				return store{}, nil, Installed{}, false, err
			}
		}
		s := store{root: root}
		unlock, err := s.lock(ctx, out)
		if err != nil {
			return store{}, nil, Installed{}, false, err
		}
		i, ok2, err := InstalledService()
		if err == nil && ok2 == ok && (!ok || i.Store == root) {
			return s, unlock, i, ok, nil
		}
		unlock()
		if err != nil {
			return store{}, nil, Installed{}, false, err
		}
		if attempt == 2 {
			return store{}, nil, Installed{}, false, errors.New("the circus worker service file keeps changing; run the command again")
		}
	}
}

// Switch is the installed worker service about to move to another package
// version: the package is in place and verified, and the store stays
// locked until Apply or Close.
type Switch struct {
	store     store
	unlock    func()
	installed Installed
	version   string
	out       io.Writer
}

// Prepare locks the store of the installed service, reads its service file
// under the lock and makes the package of version present and intact,
// without touching the service. The store is the one the service file's
// recorded environment names, whatever this process's environment says.
// The caller must Close the result (Apply does).
func Prepare(ctx context.Context, version string, opts Options) (*Switch, error) {
	s, unlock, i, ok, err := lockInstalled(ctx, opts.Out)
	if err != nil {
		return nil, err
	}
	w := &Switch{store: s, unlock: unlock, installed: i, version: version, out: opts.Out}
	if err := w.prepare(ctx, ok, opts); err != nil {
		w.Close()
		return nil, err
	}
	return w, nil
}

func (w *Switch) prepare(ctx context.Context, ok bool, opts Options) error {
	if !ok {
		return userservice.ErrNotInstalled
	}
	if err := w.installed.owns(); err != nil {
		return err
	}
	if w.installed.Version == w.version {
		return nil
	}
	return w.store.ensure(ctx, opts.Release, w.version, target, opts.Out)
}

// From is the version the service runs before the switch.
func (w *Switch) From() string { return w.installed.Version }

// Installed is the service file as read under the lock.
func (w *Switch) Installed() Installed { return w.installed }

// Apply points the service file at the prepared package, keeping its
// recorded settings, without restarting the service:
// userservice.RestartIfRunning does that, so the caller can queue the
// worker's restart with others. Versions but the new and the previous one
// are removed; the previous one stays because the running worker may still
// use it until it restarts, and the next switch removes it. On a failure
// the service file is unchanged. It closes w.
func (w *Switch) Apply() error {
	defer w.Close()
	if w.installed.Version == w.version {
		return nil
	}
	if err := userservice.Worker.Rewrite(w.store.launcher(w.version), nil, userservice.EnvironmentList(w.installed.Definition.Env)); err != nil {
		return err
	}
	if err := w.store.prune(w.version, w.installed.Version); err != nil {
		fmt.Fprintf(w.out, "Warning: could not remove other worker versions from %s: %v\n", w.store.root, err)
	}
	return nil
}

// Close unlocks the store. It may be called more than once.
func (w *Switch) Close() {
	if w.unlock != nil {
		w.unlock()
		w.unlock = nil
	}
}

// Update switches the installed worker service to the package of version:
// Prepare, Apply, then a restart if the worker runs (or stopped itself),
// which it reports.
func Update(ctx context.Context, version string, opts Options) (bool, error) {
	w, err := Prepare(ctx, version, opts)
	if err != nil {
		return false, err
	}
	same := w.From() == version
	if err := w.Apply(); err != nil || same {
		return false, err
	}
	r := userservice.RestartIfRunning(userservice.RestartRequest{Service: userservice.Worker, Rewritten: true})[0]
	return r.Requested, r.Err
}

// Uninstall stops and removes the worker service and the installed worker
// packages, leaving the store's empty directory and lock file. The
// worker's agents (<data>/agents) and credential stay. The store is the
// one the service file's environment names; a service file that runs
// something other than a package of it is refused.
func Uninstall(ctx context.Context, out io.Writer) error {
	s, unlock, i, ok, err := lockInstalled(ctx, out)
	if err != nil {
		return err
	}
	defer unlock()
	if ok {
		if err := i.owns(); err != nil {
			return err
		}
	}
	if err := userservice.Worker.Uninstall(); err != nil {
		return err
	}
	if err := s.prune(); err != nil {
		return fmt.Errorf("remove the worker packages in %s: %w", s.root, err)
	}
	return nil
}

// RecordedProxies names the proxy variables a service file holds.
func RecordedProxies(d userservice.Definition) []string {
	var names []string
	for _, name := range ProxyVariables {
		if _, ok := d.Env[name]; ok {
			names = append(names, name)
		}
	}
	return names
}

// StopFileEnv names the file where the worker and its launcher leave the
// reason of a terminal stop. `circus worker install` records it in the
// service, so the worker, its launcher and `circus worker status` agree
// on it whatever the service manager's own environment adds.
const StopFileEnv = "CIRCUS_WORKER_STOP_FILE"

// stopDir and stopName place the stop file under the data directory.
const (
	stopDir  = "worker-state"
	stopName = "stop.json"
)

// prepareStopFile returns the stop file of this process's data directory
// and creates its own directory, 0700, if the data directory exists (it
// never creates the data directory itself).
func prepareStopFile() (string, error) {
	data, err := DataDir(map[string]string{circushome.Env: os.Getenv(circushome.Env), "XDG_DATA_HOME": os.Getenv("XDG_DATA_HOME"), "HOME": os.Getenv("HOME")})
	if err != nil {
		return "", err
	}
	dir := filepath.Join(data, stopDir)
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	return filepath.Join(dir, stopName), nil
}

// StopFile is the stop file of a service file: the recorded
// CIRCUS_WORKER_STOP_FILE.
func StopFile(i Installed) string {
	if path := i.Definition.Env[StopFileEnv]; filepath.IsAbs(path) {
		return path
	}
	return ""
}

// StopReason reads why the worker service stopped itself for good, as the
// worker (or its launcher) recorded it, and says what to do. ok is false
// when nothing is recorded: the worker last started without such a stop.
func StopReason(i Installed) (string, bool) {
	path := StopFile(i)
	if path == "" {
		return "", false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	var stop struct{ Reason, Message string }
	if err := json.Unmarshal(raw, &stop); err != nil {
		return "", false
	}
	switch stop.Reason {
	case "credential-refused":
		return fmt.Sprintf("The control plane refused its credential (%s): the worker was revoked. Enroll again with `circus join --role worker --force` (it restarts the service).", stop.Message), true
	case "incompatible":
		return fmt.Sprintf("The control plane does not accept this worker (%s). Update circus with `circus update`, or the server.", stop.Message), true
	case "credential":
		return fmt.Sprintf("Its credential is missing or unusable (%s). Enroll this machine as a worker with `circus join --role worker` (it restarts the service).", stop.Message), true
	case "config":
		return fmt.Sprintf("A setting is invalid (%s). Fix it in your shell, then run `circus worker install`.", stop.Message), true
	case "node":
		return fmt.Sprintf("Its Node.js is missing or too old (%s). Install Node.js %d or later, then run `circus worker install`.", stop.Message, MinNodeMajor), true
	}
	return fmt.Sprintf("It recorded %q: %s. See the logs.", stop.Reason, stop.Message), true
}

// NodeGone reports whether the Node.js the service file records no longer
// exists (an upgrade of a version manager removed it, say).
func NodeGone(i Installed) bool {
	if i.Node == "" {
		return false
	}
	_, err := os.Stat(i.Node)
	return err != nil
}
