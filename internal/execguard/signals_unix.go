//go:build linux || darwin

package execguard

import (
	"os"
	"os/signal"

	"golang.org/x/sys/unix"
)

// forwardable preserves inherited ignored SIGHUP and SIGINT. Go installs its
// own SIGTERM handler during startup, so SIGTERM is normally forwarded.
func forwardable() []os.Signal {
	var out []os.Signal
	for _, sig := range []os.Signal{unix.SIGTERM, unix.SIGINT, unix.SIGHUP} {
		if !signal.Ignored(sig) {
			out = append(out, sig)
		}
	}
	return out
}
