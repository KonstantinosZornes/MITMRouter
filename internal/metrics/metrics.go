// Package metrics 提供零依赖的 Prometheus 文本格式指标。
// 仅覆盖本项目所需：带标签计数器、单值 gauge。
package metrics

import (
	"bytes"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

var Default = NewRegistry()

type family struct {
	help  string
	typ   string // counter | gauge
	vals  map[string]*atomic.Int64
	order []string // 保持首次出现顺序
}

type Registry struct {
	mu sync.Mutex
	m  map[string]*family
}

func NewRegistry() *Registry { return &Registry{m: map[string]*family{}} }

func labelKey(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"=\""+escapeLabel(labels[k])+"\"")
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func escapeLabel(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `"`, `\"`)
	v = strings.ReplaceAll(v, "\n", `\n`)
	return v
}

func (r *Registry) get(name, help, typ, lk string) *atomic.Int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	f, ok := r.m[name]
	if !ok {
		f = &family{help: help, typ: typ, vals: map[string]*atomic.Int64{}}
		r.m[name] = f
	}
	v, ok := f.vals[lk]
	if !ok {
		v = &atomic.Int64{}
		f.vals[lk] = v
		f.order = append(f.order, lk)
	}
	return v
}

// Inc 计数器 +1。
func (r *Registry) Inc(name, help string, labels map[string]string) {
	r.Add(name, help, labels, 1)
}

// Add 计数器 += n。
func (r *Registry) Add(name, help string, labels map[string]string, n int64) {
	r.get(name, help, "counter", labelKey(labels)).Add(n)
}

// SetGauge 设置 gauge 值。
func (r *Registry) SetGauge(name, help string, labels map[string]string, v int64) {
	r.get(name, help, "gauge", labelKey(labels)).Store(v)
}

// AddGauge gauge 增减。
func (r *Registry) AddGauge(name, help string, labels map[string]string, n int64) {
	r.get(name, help, "gauge", labelKey(labels)).Add(n)
}

// WriteText 输出 Prometheus 文本格式。
// 先在锁内渲染完整快照，再锁外写网络：慢速的 /metrics 客户端
// 不能阻塞路由热路径上的计数/打点调用。
func (r *Registry) WriteText(w io.Writer) {
	var buf bytes.Buffer
	r.mu.Lock()
	names := make([]string, 0, len(r.m))
	for n := range r.m {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		f := r.m[n]
		buf.WriteString("# HELP " + n + " " + f.help + "\n")
		buf.WriteString("# TYPE " + n + " " + f.typ + "\n")
		for _, lk := range f.order {
			s := n
			if lk != "" {
				s += lk
			}
			buf.WriteString(s + " " + itoa(f.vals[lk].Load()) + "\n")
		}
	}
	r.mu.Unlock()
	_, _ = w.Write(buf.Bytes())
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// HTTPHandler 返回可直接挂载的 /metrics 处理器。
func (r *Registry) HTTPHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		r.WriteText(w)
	}
}
