// Package ui provides module-level functionality for ui.
// input: embedded static assets (the Console under static/, the classic Vue console under classic/) and HTTP handler integration dependencies
// output: UI static file serving handlers for /ui/ (Console) and /ui-classic/ (classic Vue console, kept for one release)
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

//go:embed classic/*
var classicFS embed.FS

// Handler 返回 Console（/ui/）的嵌入式静态资源 HTTP handler。
func Handler() http.Handler {
	return subHandler(staticFS, "static")
}

// ClassicHandler 返回经典 Vue 控制台（/ui-classic/）的嵌入式静态资源 HTTP handler，保留一个版本。
func ClassicHandler() http.Handler {
	return subHandler(classicFS, "classic")
}

func subHandler(fsys embed.FS, dir string) http.Handler {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		return http.NotFoundHandler()
	}
	return http.FileServer(http.FS(sub))
}
