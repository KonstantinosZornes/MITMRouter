// Package trace 提供仅供本地排障时显式开启的明文 HTTP 追踪。
//
// 写入器为每个请求/响应事件写一行，因此请求体和响应体会在穿过本机路由时被记录，
// 不缓冲也不改变流式行为。它刻意不做任何脱敏。
package trace

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Writer 将按行组织的明文追踪事件追加到一个文件。其方法可被并发请求安全调用。
// 写入失败会被记住，此后追踪变为 no-op；调试追踪失败绝不能中断请求处理。
type Writer struct {
	file *os.File
	mu   sync.Mutex
	next atomic.Uint64
	err  error
}

// Open 以仅文件所有者可访问的权限创建或追加打开 path。
func Open(path string) (*Writer, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return nil, err
	}
	return &Writer{file: f}, nil
}

// Close 关闭底层追踪文件。应在流量监听停止接收请求后调用。
func (w *Writer) Close() error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

// Start 输出请求 URL 和全部请求头，然后返回用于流式处理请求/响应 body 的句柄。
// scheme 和 host 是已解析的出站目标，因此无论是隧道内解密还是绝对式明文请求，URL 都是绝对 URL。
func (w *Writer) Start(r *http.Request, scheme, host string) *Request {
	if w == nil {
		return nil
	}
	id := w.next.Add(1)
	url := *r.URL
	url.Scheme = scheme
	url.Host = host
	w.event(id, "request.start", fmt.Sprintf("method=%s url=%s", r.Method, strconv.Quote(url.String())))
	w.event(id, "request.headers", formatHeaders(r.Header))
	return &Request{writer: w, id: id}
}

// Request 表示一个可追踪的请求。它有意保持精简：调用方只需包装两个流式 body，
// 并在转发返回时结束它。
type Request struct {
	writer *Writer
	id     uint64
	done   sync.Once
}

// WrapRequestBody 返回一个 ReadCloser，准确记录出站 transport 消费的数据块。
// 它不会预读或缓冲 body。
func (r *Request) WrapRequestBody(body io.ReadCloser) io.ReadCloser {
	if r == nil || body == nil {
		return body
	}
	return &requestBody{ReadCloser: body, trace: r}
}

// ResponseHeader 在响应状态和响应头发送给客户端之前记录它们。
func (r *Request) ResponseHeader(status int, header http.Header) {
	if r == nil {
		return
	}
	r.writer.event(r.id, "response.start", fmt.Sprintf("status=%d", status))
	r.writer.event(r.id, "response.headers", formatHeaders(header))
}

// ResponseBody 在一个 body 数据块写入客户端后记录它。数据块会立即输出，
// 绝不在内存中累积。
func (r *Request) ResponseBody(p []byte) {
	if r == nil || len(p) == 0 {
		return
	}
	r.writer.event(r.id, "response.body", quoteBytes(p))
}

// Finish 标记请求完成。可安全地多次调用。
func (r *Request) Finish() {
	if r == nil {
		return
	}
	r.done.Do(func() { r.writer.event(r.id, "request.finish", "") })
}

type requestBody struct {
	io.ReadCloser
	trace *Request
}

func (b *requestBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.trace.writer.event(b.trace.id, "request.body", quoteBytes(p[:n]))
	}
	return n, err
}

func (b *requestBody) Close() error { return b.ReadCloser.Close() }

func (w *Writer) event(id uint64, kind, value string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil || w.file == nil {
		return
	}
	_, w.err = fmt.Fprintf(w.file, "%s id=%d %s %s\n", time.Now().UTC().Format(time.RFC3339Nano), id, kind, value)
}

func formatHeaders(h http.Header) string {
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(strconv.Quote(k))
		b.WriteString(": ")
		b.WriteString(strconv.Quote(strings.Join(h.Values(k), ", ")))
	}
	b.WriteByte('}')
	return b.String()
}

// quoteBytes 保留每个字节的可读形式，同时保持追踪文件按行组织
// （可打印文本保持可见；控制字符和二进制数据会转义）。
func quoteBytes(p []byte) string { return strconv.Quote(string(p)) }
