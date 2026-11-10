// Package toolcontext shares the execution timer signal between the dispatcher
// and tools without introducing a dependency on the default tool registry.
package toolcontext

import "context"

type timeoutKey struct{}

// WithTimeoutStarter installs the dispatcher's idempotent execution timer signal.
func WithTimeoutStarter(ctx context.Context, start func()) context.Context {
	return context.WithValue(ctx, timeoutKey{}, start)
}

// StartTimeout starts a prepared tool's execution budget. It is a no-op for
// direct callers without a dispatcher, and works through derived contexts.
func StartTimeout(ctx context.Context) {
	if start, ok := ctx.Value(timeoutKey{}).(func()); ok {
		start()
	}
}
