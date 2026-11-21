package bashtool

import "context"

// Environment returns the exported variables Bash commands run with: the
// login shell's captured exports (PATH, SSH_AUTH_SOCK, ...) plus the
// variables Bash adds. It collects the snapshot like a Bash call would,
// sharing its cache, and respects ctx; the caller owns the returned slice.
func (b *Bash) Environment(ctx context.Context) ([]string, error) {
	_, env, lease, err := b.snapshots.load(ctx)
	if err != nil {
		return nil, err
	}
	// The variables are a copy; the snapshot file itself is not needed.
	lease.release()
	return append([]string(nil), env...), nil
}
