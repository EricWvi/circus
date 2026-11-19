//go:build !webui

package webui

import "io/fs"

// FS returns nil when the Web UI is excluded from the build.
func FS() fs.FS { return nil }
