//go:build windows

package store

import (
	"context"
	"testing"
)

func TestOpenUsableOnWindows(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open on Windows: %v", err)
	}
	defer st.Close()

	if _, err := st.AllSettings(context.Background()); err != nil {
		t.Fatalf("query fresh Windows database: %v", err)
	}
}
