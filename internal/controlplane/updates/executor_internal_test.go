package updates

import (
	"context"
	"testing"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/store"
)

type capable map[string]bool

func (c capable) Supports(id, _ string) (bool, bool) { return c[id], c[id] }
func (c capable) UpdateMachine(context.Context, string, *circusv1.UpdateMachine) error {
	return nil
}
func (c capable) Updatable(id string) bool { return c[id] }

type workerSide struct{ capable }

func (w workerSide) UpdateMachine(string, *circusv1.UpdateMachine) bool { return true }

// TestExecutorFallback: with no capable role below the target, the daemon
// is chosen before the worker; offline or incapable roles never are.
func TestExecutorFallback(t *testing.T) {
	device := store.Identity{Kind: store.KindDevice, ID: "d", ClientVersion: "v2.0.0"}
	worker := store.Identity{Kind: store.KindWorker, ID: "w", ClientVersion: "v2.0.0"}
	old := store.Identity{Kind: store.KindWorker, ID: "old", ClientVersion: "v1.0.0"}
	for _, tc := range []struct {
		name string
		on   capable
		ids  []store.Identity
		want string
	}{
		{"none below: the daemon", capable{"d": true, "w": true}, []store.Identity{device, worker}, "d"},
		{"none below, daemon offline: the worker", capable{"w": true}, []store.Identity{device, worker}, "w"},
		{"an incapable role below is skipped", capable{"d": true}, []store.Identity{device, old}, "d"},
		{"nobody capable", capable{}, []store.Identity{device, worker}, ""},
	} {
		m := &Manager{cfg: Config{Daemons: tc.on, Workers: workerSide{tc.on}}}
		got, ok := m.executor(tc.ids, "v2.0.0")
		if (tc.want == "") == ok || got.ID != tc.want {
			t.Errorf("%s: executor %q (%t), want %q", tc.name, got.ID, ok, tc.want)
		}
	}
}
