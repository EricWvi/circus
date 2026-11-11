package wire_test

import (
	"flag"
	"os"
	"testing"

	"github.com/EricWvi/circus/internal/testutil/leakcheck"
	"go.uber.org/goleak"
)

// leakcheck audits sockets/fds/processes. goleak additionally audits goroutines;
// no fd/goroutine allowances are used. Heavy runs have documented RSS budgets.
func TestMain(m *testing.M) {
	flag.Parse()
	// The permanent default-options burst regression queues 60 MiB of input.
	// Its serialized copies, decoding temporaries and GC headroom exceed the
	// old small-daemon-suite 128 MiB RSS budget (observed 131.5 MiB).
	// Bound the ordinary suite at 256 MiB, using the existing race factor.
	// Actual-allocation regressions independently enforce 512 KiB / 2 MiB bounds.
	cfg := leakcheck.Config{MemoryLimit: (256 << 20) * raceMemoryFactor}
	// Go 1.26 internal/fuzz/worker.go reserves a 100 MiB shared input region
	// per worker, outside the managed Go heap. With -parallel=2 the coordinator
	// can touch both regions (observed VmHWM: 260.8 MiB); a Go heap limit cannot
	// bound them. Give fuzz infrastructure a separate 512 MiB RSS budget.
	// Targets cap reassembly inputs at 4 KiB and decode inputs at 256 KiB,
	// and check reassembly reservation
	// <= 1 KiB / 8 entries after every operation against an independent model.
	// This does not relax any production wire limit or fd/goroutine check.
	if f := flag.Lookup("test.fuzz"); f != nil && f.Value.String() != "" {
		cfg.MemoryLimit = 512 << 20
	}
	// A 64 MiB integration transfer intentionally holds the source protobuf,
	// serialized queue, reserved assembly and decoded result concurrently.
	// Race instrumentation magnifies those copies; cap this explicit run at
	// 512 MiB times the race factor instead of the 128 MiB default. No fd/goroutine
	// allowances are added, and normal runs retain the 256 MiB burst-test budget.
	if os.Getenv("CIRCUS_WIRE_LARGE_TEST") == "1" {
		cfg.MemoryLimit = (512 << 20) * raceMemoryFactor
	}
	code := leakcheck.Run(m, cfg)
	if err := goleak.Find(); err != nil {
		println(err.Error())
		code = 1
	}
	os.Exit(code)
}
