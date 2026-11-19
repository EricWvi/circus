//go:build webui

package webui

import (
	"embed"
	"io/fs"
)

//go:embed dist
var dist embed.FS

// FS returns the embedded Web UI rooted at the build output.
func FS() fs.FS {
	ui, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err)
	}
	return ui
}
