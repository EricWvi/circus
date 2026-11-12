package credfile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func sample() File {
	return File{ControlPlaneURL: "https://cp.example:8443", Role: RoleDaemon, PrincipalID: "dev-1", Token: "secret"}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "daemon.json")
	want := sample()
	want.CAFile = "/etc/circus/ca.pem"
	if err := Save(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("file mode %v, want 0600", info.Mode().Perm())
	}
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v, want 0700", dirInfo.Mode().Perm())
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("leftover temp files: %v", entries)
	}
}

func TestSaveReplacesExisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.json")
	first := sample()
	if err := Save(path, first); err != nil {
		t.Fatal(err)
	}
	second := sample()
	second.Token = "rotated"
	if err := Save(path, second); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Token != "rotated" {
		t.Fatalf("token %q, want rotated", got.Token)
	}
}

func TestLoadMissing(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "absent.json"))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err %v, want ErrNotFound", err)
	}
}

func TestLoadRejectsOpenPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.json")
	if err := Save(path, sample()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load accepted a world-readable credential file")
	}
}

func TestValidation(t *testing.T) {
	cases := map[string]func(*File){
		"unknown role":    func(f *File) { f.Role = "admin" },
		"empty url":       func(f *File) { f.ControlPlaneURL = "" },
		"empty principal": func(f *File) { f.PrincipalID = "" },
		"empty token":     func(f *File) { f.Token = "" },
		"relative ca":     func(f *File) { f.CAFile = "ca.pem" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := sample()
			mutate(&f)
			path := filepath.Join(t.TempDir(), "daemon.json")
			if err := Save(path, f); err == nil {
				t.Fatal("Save accepted an invalid file")
			}
			if err := os.WriteFile(path, []byte(`{"role":"`+string(f.Role)+`","control_plane_url":"`+f.ControlPlaneURL+`","principal_id":"`+f.PrincipalID+`","token":"`+f.Token+`","ca_file":"`+f.CAFile+`"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatal("Load accepted an invalid file")
			}
		})
	}
}

func TestPath(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/xdg")
	got, err := Path(RoleWorker)
	if err != nil {
		t.Fatal(err)
	}
	if got != "/xdg/circus/worker.json" {
		t.Fatalf("got %q", got)
	}
	t.Setenv("XDG_CONFIG_HOME", "relative")
	home := t.TempDir()
	t.Setenv("HOME", home)
	got, err = Path(RoleDaemon)
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(home, ".config", "circus", "daemon.json") {
		t.Fatalf("got %q", got)
	}
	if _, err := Path("admin"); err == nil {
		t.Fatal("Path accepted an unknown role")
	}
}
