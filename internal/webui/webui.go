// Package webui 内嵌 Vue SPA 构建产物（internal/webui/dist）。
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed all:dist
var distFS embed.FS

// Handler 返回 /ui 前缀下的 SPA 文件服务：
// 命中静态文件直接返回；未命中（SPA 深链）回退 index.html。
func Handler() http.Handler {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		panic(err)
	}
	fileServer := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}
		if _, err := fs.Stat(sub, p); err != nil {
			r.URL.Path = "/" // 回退到 index.html
		}
		fileServer.ServeHTTP(w, r)
	})
}
