package syncer

import (
	"context"
	"strings"
	"testing"
)

func TestNewSub2APIDirectReaderRejectsUnverifiedRemoteTLS(t *testing.T) {
	_, err := NewSub2APIDirectReader(context.Background(), "postgres://reader:secret@db.example/sub2api?sslmode=require")
	if err == nil || !strings.Contains(err.Error(), "sslmode=verify-full") {
		t.Fatalf("error=%v, want verify-full requirement", err)
	}
}

func TestLocalSub2APIConnectionMayUseUnixSocketOrLoopbackWithoutTLS(t *testing.T) {
	if !localPostgresHost("localhost") || !localPostgresHost("127.0.0.1") || !localPostgresHost("/var/run/postgresql") {
		t.Fatal("local PostgreSQL hosts should be recognized")
	}
	if localPostgresHost("db.example") {
		t.Fatal("remote PostgreSQL host must not be recognized as local")
	}
}
