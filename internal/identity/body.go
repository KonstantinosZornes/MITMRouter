// Package identity 从请求中解析粘滞身份。通用 header/query/Marker 优先；
// 仅在没有凭据且命中 URL Body Parser 时读取并回放 body。
package identity

import (
	"bytes"
	"io"
)

// defaultBodyLimit 是单个特殊 body parser 的默认读取上限。
const defaultBodyLimit int64 = 64 << 10

// replayBody 把已读前缀和尚未读取的原 body 拼回，保证下游读到完整原始字节。
type replayBody struct {
	io.Reader
	closer io.Closer
}

func (b replayBody) Close() error { return b.closer.Close() }

// snapshotBody 最多读取 limit 字节供身份解析，并返回用于转发的回放流。它绝不替换
// 请求的 Body 字段；调用方决定是否以及在何处挂载返回的流。
func snapshotBody(body io.ReadCloser, contentLength, limit int64) ([]byte, io.ReadCloser, bool) {
	if body == nil || limit <= 0 || contentLength > limit {
		return nil, body, false
	}

	raw, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil || int64(len(raw)) > limit {
		return nil, replayBody{
			Reader: io.MultiReader(bytes.NewReader(raw), body),
			closer: body,
		}, false
	}

	// 入站 body 已读到 EOF。这里不能关闭或替换 req.Body：调用方会转发返回的
	// 回放流，而 net/http 仍会在请求处理结束时关闭原始 body。
	return raw, io.NopCloser(bytes.NewReader(raw)), true
}
