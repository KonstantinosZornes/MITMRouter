package metrics

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func newReg() *Registry { return NewRegistry() }

func TestCounterAndGauge(t *testing.T) {
	r := newReg()
	r.Inc("c_total", "a counter", map[string]string{"upstream": "homeus"})
	r.Add("c_total", "a counter", map[string]string{"upstream": "homeus"}, 4)

	r.SetGauge("g_active", "connections", nil, 3)
	r.AddGauge("g_active", "connections", nil, -1)

	out := capture(t, r)
	if !strings.Contains(out, `c_total{upstream="homeus"} 5`) {
		t.Errorf("counter value wrong:\n%s", out)
	}
	if !strings.Contains(out, "g_active 2") {
		t.Errorf("gauge value wrong:\n%s", out)
	}
	if !strings.Contains(out, "# TYPE c_total counter") || !strings.Contains(out, "# TYPE g_active gauge") {
		t.Errorf("TYPE lines missing:\n%s", out)
	}
	if !strings.Contains(out, "# HELP g_active connections") {
		t.Errorf("HELP lines missing:\n%s", out)
	}
}

func TestLabelKeySortedDeterministic(t *testing.T) {
	a := labelKey(map[string]string{"b": "2", "a": "1"})
	b := labelKey(map[string]string{"a": "1", "b": "2"})
	if a != b {
		t.Errorf("label key not canonical: %q vs %q", a, b)
	}
	if a != `{a="1",b="2"}` {
		t.Errorf("unexpected label key %q", a)
	}
	if labelKey(nil) != "" || labelKey(map[string]string{}) != "" {
		t.Error("no labels means no braces")
	}
}

func TestLabelValueEscaping(t *testing.T) {
	got := escapeLabel(`he said "hi"\done`)
	if got != `he said \"hi\"\\done` {
		t.Errorf("escape backslash/quote broken: %q", got)
	}
	if nl := escapeLabel("a\nb"); nl != `a\nb` {
		t.Errorf("newline escape broken: %q", nl)
	}
}

func TestSeriesInsertionOrderPreserved(t *testing.T) {
	r := newReg()
	for _, u := range []string{"zeta", "alpha", "mid"} {
		r.Inc("req_total", "r", map[string]string{"u": u})
	}
	out := capture(t, r)
	i1 := strings.Index(out, `u="zeta"`)
	i2 := strings.Index(out, `u="alpha"`)
	i3 := strings.Index(out, `u="mid"`)
	if !(i1 < i2 && i2 < i3) {
		t.Errorf("series within a family must keep first-seen order:\n%s", out)
	}
}

func TestConcurrentAccess(t *testing.T) {
	r := newReg()
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r.Inc("conc_total", "c", map[string]string{"w": string(rune('A' + i%4))})
			r.SetGauge("conc_g", "g", nil, int64(i))
		}(i)
	}
	wg.Wait()
	out := capture(t, r)
	total := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "conc_total{") {
			total++
		}
	}
	if total != 4 {
		t.Errorf("expected 4 labeled series, saw %d in:\n%s", total, out)
	}
}

func TestHTTPHandler(t *testing.T) {
	r := newReg()
	r.Inc("probe_total", "p", nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	r.HTTPHandler()(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Errorf("content-type=%q", ct)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("probe_total 1")) {
		t.Errorf("body=%s", rec.Body.String())
	}
}

// 慢/挂死的 /metrics 客户端不得阻塞热路径打点：
// WriteText 在网络写阻塞期间，Inc 必须照常完成。
func TestWriteTextDoesNotHoldLockWhileWriting(t *testing.T) {
	r := newReg()
	r.Inc("slow_total", "s", nil)

	entered := make(chan struct{}) // writer 已进入 Write
	release := make(chan struct{})
	w := &blockedWriter{entered: entered, release: release}

	done := make(chan struct{})
	go func() { r.WriteText(w); close(done) }()
	<-entered

	incDone := make(chan struct{})
	go func() { r.Inc("slow_total", "s", nil); close(incDone) }()
	select {
	case <-incDone: // 打点未被写阻塞 ✓
	case <-time.After(2 * time.Second):
		t.Fatal("Inc blocked behind a stuck metrics writer")
	}

	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("WriteText did not finish after release")
	}
}

type blockedWriter struct {
	entered, release chan struct{}
	once             sync.Once
}

func (w *blockedWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	return len(p), nil
}

func capture(t *testing.T, r *Registry) string {
	t.Helper()
	var buf bytes.Buffer
	r.WriteText(&buf)
	return buf.String()
}
