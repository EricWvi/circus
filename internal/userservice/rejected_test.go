package userservice

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/EricWvi/circus/internal/credfile"
)

// rejectedFixture installs svc with the fake managers, reading
// the default credential of its role (or other, when given), and saves that
// default credential. It returns the default credential's path.
func rejectedFixture(t *testing.T, f serviceFixture, svc *Service, other string) string {
	t.Helper()
	path, err := credfile.Path(svc.role)
	if err != nil {
		t.Fatal(err)
	}
	saveCredential(t, path, svc.role)
	args := []string{svc.name, "--service"}
	if other != "" {
		args = append(args, "--credentials", other)
	}
	env := []string{"XDG_CONFIG_HOME=" + f.config}
	m, err := svc.current()
	if err != nil {
		t.Fatal(err)
	}
	var text string
	switch m := m.(type) {
	case systemd:
		text = m.unitText(fakeExecutable, args, env)
	case launchd:
		text = m.plist(fakeExecutable, args, env)
	}
	if err := writePrivate(m.file(), text); err != nil {
		t.Fatal(err)
	}
	if got := m.definition([]byte(text)).Credentials(svc.role); other == "" && got != path {
		t.Fatalf("the service file reads %s, want %s", got, path)
	}
	return path
}

// saved names the credential saved at path, as the process that read it
// would.
func saved(t *testing.T, path string) Rejected {
	t.Helper()
	f, err := credfile.Load(path)
	if err != nil {
		return Rejected{PrincipalID: "missing"}
	}
	return RejectedCredential(f)
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return err == nil
}

// On Linux the service removes itself without stopping its unit (which
// would kill the caller): it disables it, removes the unit file and reloads
// systemd, then deletes the credential.
func TestRemoveRejectedSystemd(t *testing.T) {
	for _, svc := range []*Service{Daemon, Worker} {
		t.Run(svc.name, func(t *testing.T) {
			f := fakeService(t, "linux")
			path := rejectedFixture(t, f, svc, "")
			m, _ := svc.current()
			removed, err := svc.RemoveRejected(path, saved(t, path), 4242)
			if err != nil || !removed {
				t.Fatalf("RemoveRejected = %v, %v", removed, err)
			}
			if exists(t, m.file()) || exists(t, path) || len(asideFiles(t, path)) != 0 {
				t.Fatalf("unit file kept %v, credential kept %v, aside %q", exists(t, m.file()), exists(t, path), asideFiles(t, path))
			}
			f.checkCalls(t, "systemctl --user is-system-running", "systemctl --user disable "+svc.unitName(), "systemctl --user daemon-reload")
		})
	}
}

// On macOS the plist goes at once and a detached helper, started for the
// service's own process, boots the agent out once that process has exited.
func TestRemoveRejectedLaunchd(t *testing.T) {
	for _, svc := range []*Service{Daemon, Worker} {
		t.Run(svc.name, func(t *testing.T) {
			f := fakeService(t, "darwin")
			path := rejectedFixture(t, f, svc, "")
			var started []string
			old := startDetachedUnload
			t.Cleanup(func() { startDetachedUnload = old })
			startDetachedUnload = func(home string, s *Service, pid int) error {
				started = append(started, home+" "+s.name+" "+strconv.Itoa(pid))
				return nil
			}
			m, _ := svc.current()
			removed, err := svc.RemoveRejected(path, saved(t, path), 4242)
			if err != nil || !removed {
				t.Fatalf("RemoveRejected = %v, %v", removed, err)
			}
			if exists(t, m.file()) || exists(t, path) || len(asideFiles(t, path)) != 0 {
				t.Fatalf("plist kept %v, credential kept %v, aside %q", exists(t, m.file()), exists(t, path), asideFiles(t, path))
			}
			if want := f.home + " " + svc.name + " 4242"; len(started) != 1 || started[0] != want {
				t.Fatalf("helpers started: %q, want %q", started, want)
			}
			f.checkCalls(t)
		})
	}
}

// Nothing is removed when the installed service reads another credential
// (a newer install's), when none is installed, or off Linux and macOS.
func TestRemoveRejectedKeepsAnotherCredentialsService(t *testing.T) {
	for _, platform := range []string{"linux", "darwin"} {
		t.Run(platform, func(t *testing.T) {
			f := fakeService(t, platform)
			old := startDetachedUnload
			t.Cleanup(func() { startDetachedUnload = old })
			startDetachedUnload = func(string, *Service, int) error { t.Error("helper started"); return nil }
			other := filepath.Join(f.dir, "rejoined", "daemon.json")
			path := rejectedFixture(t, f, Daemon, other)
			m, _ := Daemon.current()
			if removed, err := Daemon.RemoveRejected(path, saved(t, path), 1); removed || err != nil {
				t.Fatalf("RemoveRejected = %v, %v", removed, err)
			}
			if !exists(t, m.file()) || !exists(t, path) {
				t.Fatal("another credential's service, or the credential, was removed")
			}
			if err := os.Remove(m.file()); err != nil {
				t.Fatal(err)
			}
			if removed, err := Daemon.RemoveRejected(path, saved(t, path), 1); removed || err != nil || !exists(t, path) {
				t.Fatalf("without a service: %v, %v", removed, err)
			}
			f.checkCalls(t)
		})
	}
	f := fakeService(t, "freebsd")
	if removed, err := Daemon.RemoveRejected(filepath.Join(f.dir, "daemon.json"), Rejected{PrincipalID: "device"}, 1); removed || err != nil {
		t.Fatalf("unsupported platform: %v, %v", removed, err)
	}
}

// asideFiles lists the moved-aside credential files left next to path.
func asideFiles(t *testing.T, path string) []string {
	t.Helper()
	matches, err := filepath.Glob(path + ".revoked-*")
	if err != nil {
		t.Fatal(err)
	}
	return matches
}

func hook(t *testing.T, at *func(), fn func()) {
	t.Helper()
	old := *at
	t.Cleanup(func() { *at = old })
	*at = fn
}

// A join since the process started (a rejoin's `join --force`) wrote a new
// credential to the same path: the moved file is not the rejected one, so
// it goes back, and the service, now that credential's, stays; on either
// platform, for daemon and worker, whether or not the principal changed.
func TestRemoveRejectedKeepsANewerCredential(t *testing.T) {
	for _, platform := range []string{"linux", "darwin"} {
		for _, svc := range []*Service{Daemon, Worker} {
			t.Run(platform+"/"+svc.name, func(t *testing.T) {
				f := fakeService(t, platform)
				old := startDetachedUnload
				t.Cleanup(func() { startDetachedUnload = old })
				startDetachedUnload = func(string, *Service, int) error { t.Error("helper started"); return nil }
				path := rejectedFixture(t, f, svc, "")
				running := saved(t, path)
				for _, next := range []credfile.File{
					{Role: svc.role, PrincipalID: "id-2", Token: "new-token", ControlPlaneURL: "https://circus.example"},
					{Role: svc.role, PrincipalID: "device", Token: "new-token", ControlPlaneURL: "https://circus.example"},
				} {
					if err := credfile.Save(path, next); err != nil {
						t.Fatal(err)
					}
					m, _ := svc.current()
					if removed, err := svc.RemoveRejected(path, running, 1); removed || err != nil {
						t.Fatalf("RemoveRejected = %v, %v", removed, err)
					}
					if !exists(t, m.file()) || saved(t, path) != RejectedCredential(next) || len(asideFiles(t, path)) != 0 {
						t.Fatalf("service kept %v, credential %v, aside %q", exists(t, m.file()), saved(t, path), asideFiles(t, path))
					}
				}
				f.checkCalls(t)
			})
		}
	}
}

// While the moved file (not the rejected one) is aside, a still newer
// credential is written to the path: the newer one stays, the moved one,
// outdated, is dropped, and the service stays.
func TestRemoveRejectedNewerCredentialWhileAside(t *testing.T) {
	for _, svc := range []*Service{Daemon, Worker} {
		t.Run(svc.name, func(t *testing.T) {
			f := fakeService(t, "linux")
			path := rejectedFixture(t, f, svc, "")
			running := saved(t, path)
			between := credfile.File{Role: svc.role, PrincipalID: "id-2", Token: "between", ControlPlaneURL: "https://circus.example"}
			newest := credfile.File{Role: svc.role, PrincipalID: "id-3", Token: "newest", ControlPlaneURL: "https://circus.example"}
			if err := credfile.Save(path, between); err != nil {
				t.Fatal(err)
			}
			hook(t, &testHookAside, func() {
				if err := credfile.Save(path, newest); err != nil {
					t.Error(err)
				}
			})
			m, _ := svc.current()
			if removed, err := svc.RemoveRejected(path, running, 1); removed || err != nil {
				t.Fatalf("RemoveRejected = %v, %v", removed, err)
			}
			if !exists(t, m.file()) || saved(t, path) != RejectedCredential(newest) || len(asideFiles(t, path)) != 0 {
				t.Fatalf("service kept %v, credential %v, aside %q", exists(t, m.file()), saved(t, path), asideFiles(t, path))
			}
			f.checkCalls(t)
		})
	}
}

// The moved file is the rejected one, and a join writes a new credential
// while the service is removed: the service file is written back and the
// unit enabled again, and the error says to run the install command.
func TestRemoveRejectedJoinRacesTheRemoval(t *testing.T) {
	for _, svc := range []*Service{Daemon, Worker} {
		t.Run(svc.name, func(t *testing.T) {
			f := fakeService(t, "linux")
			path := rejectedFixture(t, f, svc, "")
			running := saved(t, path)
			m, _ := svc.current()
			unit, err := os.ReadFile(m.file())
			if err != nil {
				t.Fatal(err)
			}
			joined := credfile.File{Role: svc.role, PrincipalID: "id-2", Token: "joined", ControlPlaneURL: "https://circus.example"}
			hook(t, &testHookAside, func() {
				if err := credfile.Save(path, joined); err != nil {
					t.Error(err)
				}
			})
			removed, err := svc.RemoveRejected(path, running, 1)
			if !removed || err == nil || !strings.Contains(err.Error(), "run `"+svc.installCommand()+"`") {
				t.Fatalf("RemoveRejected = %v, %v", removed, err)
			}
			if got, _ := os.ReadFile(m.file()); string(got) != string(unit) {
				t.Fatalf("service file not written back:\n%s", got)
			}
			if saved(t, path) != RejectedCredential(joined) || len(asideFiles(t, path)) != 0 {
				t.Fatalf("credential %v, aside %q", saved(t, path), asideFiles(t, path))
			}
			f.checkCalls(t, "systemctl --user is-system-running", "systemctl --user disable "+svc.unitName(), "systemctl --user daemon-reload",
				"systemctl --user daemon-reload", "systemctl --user enable "+svc.unitName())
		})
	}
}

// The helper's unload step waits for the service's process to exit, then
// boots the agent out and waits for it to unload.
func TestLaunchReloadUnloadStep(t *testing.T) {
	f := fakeService(t, "darwin")
	f.response(t, "loaded", "")
	proc := exec.Command("/bin/sleep", "0.3")
	if err := proc.Start(); err != nil {
		t.Fatal(err)
	}
	pid := proc.Process.Pid
	exited := make(chan struct{})
	go func() { _ = proc.Wait(); close(exited) }()
	if code := RunLaunchReload([]string{f.home, "worker=unload:" + strconv.Itoa(pid)}, io.Discard); code != 0 {
		t.Fatalf("unload exit code %d", code)
	}
	select {
	case <-exited:
	default:
		t.Fatal("booted out before the service's process exited")
	}
	data, err := os.ReadFile(filepath.Join(f.dir, "calls"))
	if err != nil || !strings.HasPrefix(string(data), "launchctl bootout "+launchd{svc: Worker}.target()+"\n") || exists(t, filepath.Join(f.dir, "loaded")) {
		t.Fatalf("calls:\n%s (%v)", data, err)
	}
	for _, bad := range []string{"worker=unload", "worker=unload:x", "worker=load:5"} {
		if code := RunLaunchReload([]string{f.home, bad}, io.Discard); code != reloadUnloadFailure {
			t.Errorf("%s: exit code %d", bad, code)
		}
	}
}
