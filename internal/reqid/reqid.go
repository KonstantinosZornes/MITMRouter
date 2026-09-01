// Package reqid 提供按请求作用域生成的加密安全随机关联 ID。
package reqid

import (
	"context"
	"crypto/rand"
	"encoding/hex"
)

type contextKey struct{}

// New 返回随机的小写十六进制请求 ID。
// crypto/rand 失败表示主机无法安全生成请求 ID。
func New() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// With 返回携带 id 的子上下文。
func With(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, contextKey{}, id)
}

// From 返回 ctx 携带的请求 ID；不存在时返回空字符串。
func From(ctx context.Context) string {
	id, _ := ctx.Value(contextKey{}).(string)
	return id
}

// Ensure 为 ctx 附加新生成的 ID；若已有 ID 则保持不变。
func Ensure(ctx context.Context) (context.Context, string) {
	if id := From(ctx); id != "" {
		return ctx, id
	}
	id := New()
	return With(ctx, id), id
}
