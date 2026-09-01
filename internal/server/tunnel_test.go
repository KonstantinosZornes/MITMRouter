package server

import (
	"bufio"
	"net"
	"testing"
)

type closeWriteRecorder struct {
	net.Conn
	called chan struct{}
}

func (c *closeWriteRecorder) CloseWrite() error {
	close(c.called)
	return nil
}

// P1-2: bufConn 包装后仍必须暴露底层 TCP 连接的半关闭能力。
func TestBufConnForwardsCloseWrite(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	called := make(chan struct{})
	wrapped := &bufConn{
		Conn: &closeWriteRecorder{Conn: server, called: called},
		r:    bufio.NewReader(server),
	}
	cw, ok := any(wrapped).(interface{ CloseWrite() error })
	if !ok {
		t.Fatal("bufConn must preserve CloseWrite capability")
	}
	if err := cw.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}
	select {
	case <-called:
	default:
		t.Fatal("bufConn did not forward CloseWrite to underlying connection")
	}
}
