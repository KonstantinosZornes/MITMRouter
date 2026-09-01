// mock 上游出口：开发/集成测试用。
// 记录收到的 CONNECT 目标与 Proxy-Authorization 到日志文件后透明转发。
// 用法: mockexit [logfile]  (默认 ./mock.log，监听 127.0.0.1:18080)
package main

import (
	"io"
	"log"
	"net"
	"net/http"
	"os"
)

func main() {
	logPath := "./mock.log"
	if len(os.Args) > 1 {
		logPath = os.Args[1]
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		panic(err)
	}
	logger := log.New(f, "", 0)

	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			logger.Printf("CONNECT %s auth=%q", r.Host, r.Header.Get("Proxy-Authorization"))
			hj, ok := w.(http.Hijacker)
			if !ok {
				return
			}
			conn, rw, err := hj.Hijack()
			if err != nil {
				return
			}
			rw.Writer.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
			rw.Writer.Flush()
			dst, err := net.Dial("tcp", r.Host)
			if err != nil {
				conn.Close()
				return
			}
			go func() { io.Copy(dst, conn); dst.Close() }()
			io.Copy(conn, dst)
			conn.Close()
			return
		}
		// 绝对式明文请求：记录后经默认 Transport 转发
		logger.Printf("ABS %s auth=%q", r.URL.Host, r.Header.Get("Proxy-Authorization"))
		out := r.Clone(r.Context())
		out.RequestURI = ""
		resp, err := http.DefaultTransport.RoundTrip(out)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for k, vv := range resp.Header {
			for _, v := range vv {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	})
	addr := "127.0.0.1:18080"
	if len(os.Args) > 2 {
		addr = os.Args[2]
	}
	if err := http.ListenAndServe(addr, h); err != nil {
		panic(err)
	}
}
