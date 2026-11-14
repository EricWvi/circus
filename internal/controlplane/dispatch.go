//go:build !e2efault

package controlplane

import "github.com/EricWvi/circus/internal/controlplane/calls"

func dispatchSender(s calls.Sender) calls.Sender { return s }
