package api

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// ctxBG 简写。
func ctxBG() context.Context { return context.Background() }

func urlParse(raw string) (*url.URL, error)     { return url.Parse(raw) }
func urlUserPassword(u, p string) *url.Userinfo { return url.UserPassword(u, p) }
func urlUser(u string) *url.Userinfo            { return url.User(u) }

// randHex n 字节随机数的 hex 编码（长度 2n）。
func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func remoteHostStr(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}

func boolPtr(b bool) *bool { return &b }

func nullStr(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

// ---------- 凭据兼容语义 ----------

const (
	maskToken = "____"
	keepToken = "__unchanged__"
)

// mergeAuth 处理 PUT settings 提交的 listen_auth：
// 旧客户端提交密码段为 ____/__unchanged__ 时仍沿用旧密码。
func mergeAuth(old, new string) string {
	if new == "" {
		return ""
	}
	if old == "" {
		return new
	}
	ni := strings.IndexByte(new, ':')
	oi := strings.IndexByte(old, ':')
	if ni < 0 || oi < 0 {
		return new
	}
	pwd := new[ni+1:]
	if pwd == maskToken || pwd == keepToken {
		return new[:ni+1] + old[oi+1:]
	}
	return new
}

// failInternal 统一内部错误出口：详情进服务端日志，对外固定文案。
func (a *API) failInternal(w http.ResponseWriter, r *http.Request, err error) {
	a.logger().Log(r.Context(), slog.LevelError, "api internal", "path", r.URL.Path, "err", err)
	writeErr(w, http.StatusInternalServerError, "internal", "internal error")
}
