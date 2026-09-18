// Package web carries the templates and static assets. They are embedded in the
// binary; THESES_DEV=1 makes the server read them from this directory instead.
package web

import (
	"embed"
	"io/fs"
)

//go:embed templates static
var files embed.FS

// Templates and Static are the two trees the server renders and serves from.
var (
	Templates = must(fs.Sub(files, "templates"))
	Static    = must(fs.Sub(files, "static"))
)

// DevDir is where THESES_DEV=1 reads them from, relative to the working directory.
const DevDir = "web"

func must(f fs.FS, err error) fs.FS {
	if err != nil {
		panic(err)
	}
	return f
}
