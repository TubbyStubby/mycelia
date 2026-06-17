// Package web embeds the static single-page frontend.
package web

import (
	"embed"
	"io/fs"
)

//go:embed static
var assets embed.FS

// FS returns the embedded static asset filesystem rooted at the static dir.
func FS() fs.FS {
	sub, err := fs.Sub(assets, "static")
	if err != nil {
		panic(err)
	}
	return sub
}
