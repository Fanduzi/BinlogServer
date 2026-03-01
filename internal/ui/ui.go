// Package ui provides module-level functionality for ui.
// input: embedded static assets and HTTP handler integration dependencies
// output: UI static file serving handler for browser-based management console
// pos: UI delivery adapter exposing bundled frontend assets from backend process
// note: if this file changes, update this header and module README.md.
package ui

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static/*
var staticFS embed.FS

// Handler 返回嵌入式前端静态资源的 HTTP handler。
func Handler() http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		return http.NotFoundHandler()
	}
	return http.FileServer(http.FS(sub))
}
