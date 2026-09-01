package trace

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriterStreamsPlaintextRequestAndResponse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests.log")
	w, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequest(http.MethodPost, "/v1/chat", io.NopCloser(strings.NewReader(`{"prompt":"你好"}`)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer secret-value")
	tr := w.Start(req, "https", "api.example.test")
	body, err := io.ReadAll(tr.WrapRequestBody(req.Body))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != `{"prompt":"你好"}` {
		t.Fatalf("wrapped request body = %q", body)
	}
	tr.ResponseHeader(http.StatusCreated, http.Header{"X-Trace": {"all-visible"}})
	tr.ResponseBody([]byte(`{"result":"ok"}`))
	tr.Finish()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	for _, want := range []string{
		`request.start method=POST url="https://api.example.test/v1/chat"`,
		`request.headers {"Authorization": "Bearer secret-value"}`,
		`request.body "{\"prompt\":\"你好\"}"`,
		"response.start status=201",
		`response.headers {"X-Trace": "all-visible"}`,
		`response.body "{\"result\":\"ok\"}"`,
		"request.finish",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("trace is missing %q:\n%s", want, text)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("trace mode = %o, want 600", info.Mode().Perm())
	}
}

func TestOpenRestrictsExistingFilePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "existing.log")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	w, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("existing trace mode = %o, want 600", info.Mode().Perm())
	}
}

func TestNilRequestTraceLeavesBodyUntouched(t *testing.T) {
	body := io.NopCloser(strings.NewReader("body"))
	var tr *Request
	if tr.WrapRequestBody(body) != body {
		t.Error("disabled trace must not wrap the request body")
	}
}
