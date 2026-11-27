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
// default credential. It returns the default credential's path. The daemon
// reads other through --credentials; the worker, which has no such
// argument, through a CIRCUS_HOME of which other is config/worker.json.
func rejectedFixture(t *testing.T, f serviceFixture, svc *Service, other string) string {
	t.Helper()
	path, err := credfile.Path(svc.role)
	if err != nil {
		t.Fatal(err)
	}
	saveCredential(t, path, svc.role)
	args := []string{svc.name, "--service"}
	env := []string{"XDG_CONFIG_HOME=" + f.config}
	switch {
	case other == "":
	case svc.role == credfile.RoleWorker:
		if filepath.Base(filepath.Dir(other)) != "config" || filepath.Base(other) != "worker.json" {
			t.Fatalf("the worker reads <CIRCUS_HOME>/config/worker.json, not %s", other)
		}
		env = append(env, "CIRCUS_HOME="+filepath.Dir(filepath.Dir(other)))
	default:
		args = append(args, "--credentials", other)
	}
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

// A process run in the foreground removes only its credential file, and
// only while it holds the rejected credential and no installed service of
// its role reads it: never a service.
func TestRemoveRejectedCredential(t *testing.T) {
	for _, svc := range []*Service{Daemon, Worker} {
		t.Run(svc.name, func(t *testing.T) {
			f := fakeService(t, "linux")
			other := filepath.Join(f.dir, "rejoined", "config", string(svc.role)+".json")
			path := rejectedFixture(t, f, svc, other)
			m, _ := svc.current()
			unit := readPrivate(t, m.file())
			running := saved(t, path)
			outcome, err := svc.RemoveRejectedCredential(path, running)
			if err != nil || outcome != CredentialRemoved {
				t.Fatalf("RemoveRejectedCredential = %v, %v", outcome, err)
			}
			if exists(t, path) || len(asideFiles(t, path)) != 0 || readPrivate(t, m.file()) != unit {
				t.Fatalf("credential kept %v, aside %q, service file changed %v", exists(t, path), asideFiles(t, path), readPrivate(t, m.file()) != unit)
			}
			// The file is gone now: nothing to do.
			if outcome, err := svc.RemoveRejectedCredential(path, running); outcome != CredentialGone || err != nil || exists(t, path) {
				t.Fatalf("missing file: %v, %v", outcome, err)
			}
			// Without a service, or on a platform without a service manager.
			if err := os.Remove(m.file()); err != nil {
				t.Fatal(err)
			}
			saveCredential(t, path, svc.role)
			if outcome, err := svc.RemoveRejectedCredential(path, running); outcome != CredentialRemoved || err != nil || exists(t, path) {
				t.Fatalf("without a service: %v, %v", outcome, err)
			}
			goos = "freebsd"
			saveCredential(t, path, svc.role)
			if outcome, err := svc.RemoveRejectedCredential(path, running); outcome != CredentialRemoved || err != nil || exists(t, path) {
				t.Fatalf("unsupported platform: %v, %v", outcome, err)
			}
			f.checkCalls(t)
		})
	}
}

// The installed service of the same role reads the credential file: the
// file is left to the service, which removes both on the same proof, and
// the service file is not changed; on either platform.
func TestRemoveRejectedCredentialLeavesTheServicesFile(t *testing.T) {
	for _, platform := range []string{"linux", "darwin"} {
		for _, svc := range []*Service{Daemon, Worker} {
			t.Run(platform+"/"+svc.name, func(t *testing.T) {
				f := fakeService(t, platform)
				path := rejectedFixture(t, f, svc, "")
				m, _ := svc.current()
				unit := readPrivate(t, m.file())
				before := readPrivate(t, path)
				if outcome, err := svc.RemoveRejectedCredential(path, saved(t, path)); outcome != CredentialLeftToService || err != nil {
					t.Fatalf("RemoveRejectedCredential = %v, %v", outcome, err)
				}
				if readPrivate(t, path) != before || readPrivate(t, m.file()) != unit || len(asideFiles(t, path)) != 0 {
					t.Fatalf("credential or service file changed, aside %q", asideFiles(t, path))
				}
				f.checkCalls(t)
			})
		}
	}
}

// The installed service reads the same file by another path, through a
// symbolic link to its directory: the file is left to the service.
func TestRemoveRejectedCredentialSameFileByAnotherPath(t *testing.T) {
	for _, svc := range []*Service{Daemon, Worker} {
		t.Run(svc.name, func(t *testing.T) {
			f := fakeService(t, "linux")
			real := filepath.Join(f.dir, "real")
			alias := filepath.Join(f.dir, "alias")
			if err := os.MkdirAll(filepath.Join(real, "config"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(real, alias); err != nil {
				t.Fatal(err)
			}
			// The service reads real/config/<role>.json; the foreground
			// process reads it as alias/config/<role>.json.
			rejectedFixture(t, f, svc, filepath.Join(real, "config", string(svc.role)+".json"))
			path := filepath.Join(alias, "config", string(svc.role)+".json")
			saveCredential(t, path, svc.role)
			before := readPrivate(t, path)
			if outcome, err := svc.RemoveRejectedCredential(path, saved(t, path)); outcome != CredentialLeftToService || err != nil {
				t.Fatalf("RemoveRejectedCredential = %v, %v", outcome, err)
			}
			if readPrivate(t, path) != before || len(asideFiles(t, path)) != 0 {
				t.Fatalf("credential changed, aside %q", asideFiles(t, path))
			}
			f.checkCalls(t)
		})
	}
}

// An installed service file that is readable but records no credential
// file (it cannot be parsed, has no program, or records neither
// --credentials nor CIRCUS_HOME, XDG_CONFIG_HOME or HOME): the file is
// kept, never taken for the default path.
func TestRemoveRejectedCredentialKeepsTheFileForAnUnparsedService(t *testing.T) {
	for _, tc := range []struct{ platform, text string }{
		{"linux", "[Service]\nEnvironment=\"CIRCUS_HOME=/elsewhere\"\n"},
		{"linux", "[Service]\nExecStart=\"/opt/circus\" \"daemon\" \"--service\"\n"},
		{"darwin", "<?xml version=\"1.0\"?>\n<plist version=\"1.0\"><dict><key>ProgramArguments</key><array><string>/opt/circus</string>"},
	} {
		t.Run(tc.platform, func(t *testing.T) {
			f := fakeService(t, tc.platform)
			path := rejectedFixture(t, f, Daemon, filepath.Join(f.dir, "rejoined", "daemon.json"))
			m, _ := Daemon.current()
			if err := writePrivate(m.file(), tc.text); err != nil {
				t.Fatal(err)
			}
			outcome, err := Daemon.RemoveRejectedCredential(path, saved(t, path))
			if outcome != CredentialKept || err == nil || !strings.Contains(err.Error(), "so it is kept") || !exists(t, path) {
				t.Fatalf("RemoveRejectedCredential = %v, %v", outcome, err)
			}
			if readPrivate(t, m.file()) != tc.text {
				t.Fatal("the service file changed")
			}
			f.checkCalls(t)
		})
	}
}

// When it cannot be told whether the installed service reads the file, the
// file is kept.
func TestRemoveRejectedCredentialKeepsTheFileWhenTheServiceIsUnknown(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads any file")
	}
	f := fakeService(t, "linux")
	path := rejectedFixture(t, f, Daemon, filepath.Join(f.dir, "rejoined", "daemon.json"))
	m, _ := Daemon.current()
	if err := os.Chmod(m.file(), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(m.file(), 0o600) })
	outcome, err := Daemon.RemoveRejectedCredential(path, saved(t, path))
	if outcome != CredentialKept || err == nil || !strings.Contains(err.Error(), "so it is kept") || !exists(t, path) {
		t.Fatalf("RemoveRejectedCredential = %v, %v", outcome, err)
	}
	f.checkCalls(t)
}

// A join wrote another credential to the path (another principal, or the
// same principal with a new token), or it is unreadable: it is put back
// unchanged.
func TestRemoveRejectedCredentialKeepsAnotherCredential(t *testing.T) {
	fakeService(t, "linux")
	path := filepath.Join(t.TempDir(), "config", "daemon.json")
	saveCredential(t, path, credfile.RoleDaemon)
	running := saved(t, path)
	for _, next := range []string{
		`{"role":"daemon","principal_id":"id-2","token":"test-token","control_plane_url":"https://circus.example"}`,
		`{"role":"daemon","principal_id":"device","token":"new-token","control_plane_url":"https://circus.example"}`,
		`{"role":"daemon",`,
	} {
		if err := os.WriteFile(path, []byte(next), 0o600); err != nil {
			t.Fatal(err)
		}
		if outcome, err := Daemon.RemoveRejectedCredential(path, running); outcome != CredentialKept || err != nil {
			t.Fatalf("RemoveRejectedCredential = %v, %v", outcome, err)
		}
		if after := readPrivate(t, path); after != next || len(asideFiles(t, path)) != 0 {
			t.Fatalf("credential %q, want %q; aside %q", after, next, asideFiles(t, path))
		}
	}
	// An empty principal names no credential.
	if outcome, err := Daemon.RemoveRejectedCredential(path, Rejected{}); outcome != CredentialKept || err != nil || !exists(t, path) {
		t.Fatalf("empty principal: %v, %v", outcome, err)
	}
}

// While the moved file (not the rejected one) is aside, a still newer
// credential is written to the path: the newer one stays and the moved one,
// outdated, is dropped.
func TestRemoveRejectedCredentialNewerWhileAside(t *testing.T) {
	fakeService(t, "linux")
	path := filepath.Join(t.TempDir(), "config", "worker.json")
	saveCredential(t, path, credfile.RoleWorker)
	running := saved(t, path)
	between := credfile.File{Role: credfile.RoleWorker, PrincipalID: "id-2", Token: "between", ControlPlaneURL: "https://circus.example"}
	newest := credfile.File{Role: credfile.RoleWorker, PrincipalID: "id-3", Token: "newest", ControlPlaneURL: "https://circus.example"}
	if err := credfile.Save(path, between); err != nil {
		t.Fatal(err)
	}
	hook(t, &testHookAside, func() {
		if err := credfile.Save(path, newest); err != nil {
			t.Error(err)
		}
	})
	if outcome, err := Worker.RemoveRejectedCredential(path, running); outcome != CredentialKept || err != nil {
		t.Fatalf("RemoveRejectedCredential = %v, %v", outcome, err)
	}
	if saved(t, path) != RejectedCredential(newest) || len(asideFiles(t, path)) != 0 {
		t.Fatalf("credential %v, aside %q", saved(t, path), asideFiles(t, path))
	}
}
