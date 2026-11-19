package circushome_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/EricWvi/circus/internal/circushome"
)

func TestDir(t *testing.T) {
	t.Setenv(circushome.Env, "")
	if d, err := circushome.Dir("config"); err != nil || d != "" {
		t.Fatalf("unset: %q %v", d, err)
	}
	t.Setenv(circushome.Env, "/srv/circus-dev")
	if d, err := circushome.Dir("config"); err != nil || d != "/srv/circus-dev/config" {
		t.Fatalf("absolute: %q %v", d, err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(circushome.Env, ".tmp/dev")
	if d, err := circushome.Dir("state"); err != nil || d != filepath.Join(wd, ".tmp/dev/state") {
		t.Fatalf("relative: %q %v", d, err)
	}
}
