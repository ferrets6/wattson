package main

import (
	"bytes"
	"embed"
	"html"
	"io/fs"
	"net/http"
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

const baseTitle = "Wattson"

func pageTitle(siteName string) string {
	if siteName == "" {
		return baseTitle
	}
	return baseTitle + " · " + siteName
}

func renderIndex(root fs.FS, siteName string) ([]byte, error) {
	page, err := fs.ReadFile(root, "index.html")
	if err != nil {
		return nil, err
	}
	return bytes.Replace(page,
		[]byte("<title>"+baseTitle+"</title>"),
		[]byte("<title>"+html.EscapeString(pageTitle(siteName))+"</title>"),
		1), nil
}

func indexHandler(page []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(page)
	}
}
