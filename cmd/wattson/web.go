package main

import (
	"embed"
	"io/fs"
)

//go:embed web
var webFS embed.FS

func webRoot() fs.FS {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		panic("cmd/wattson: web/ embed is broken: " + err.Error())
	}
	return sub
}
