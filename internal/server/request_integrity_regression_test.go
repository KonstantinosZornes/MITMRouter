package server

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAuthenticatedAbsoluteRequestDoesNotLeakProxyAuthorization(t *testing.T) {
	s := newStack(t)
	snap := s.holder.Current()
	snap.ListenAuth = "internal-user:internal-pass"
	s.holder.Set(snap)
	received := make(chan string, 1)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.Header.Get("Proxy-Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer origin.Close()
	req, err := http.NewRequest(http.MethodGet, origin.URL+"/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(snap.ListenAuth)))
	resp, err := ingressClient(t, s.feURL).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if got := <-received; got != "" {
		t.Fatalf("origin received ingress credential %q", got)
	}
}

func TestTransportPreservesCompression(t *testing.T) {
	acceptEncoding := make(chan string, 1)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		acceptEncoding <- r.Header.Get("Accept-Encoding")
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		_, _ = gz.Write([]byte("compressed-body"))
		_ = gz.Close()
	}))
	defer origin.Close()
	s := newStack(t)
	req, err := http.NewRequest(http.MethodGet, origin.URL+"/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := s.srv.transport.RoundTrip(cloneForwardRequest(req, "http", req.URL.Host, req.Context()))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if got := <-acceptEncoding; got != "" {
		t.Errorf("Accept-Encoding injected: %q", got)
	}
	if got := resp.Header.Get("Content-Encoding"); got != "gzip" {
		t.Errorf("Content-Encoding changed: %q", got)
	}
	if !bytes.HasPrefix(body, []byte{0x1f, 0x8b}) {
		t.Errorf("response body was transparently decoded: %q", body)
	}
}
