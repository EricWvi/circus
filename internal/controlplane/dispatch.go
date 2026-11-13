//go:build !e2efault

package controlplane

import "github.com/EricWvi/circus/internal/controlplane/bus"

func dispatchBus(b bus.Bus) bus.Bus { return b }
