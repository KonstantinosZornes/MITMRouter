package syncer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseCPAAuthFile(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		want      *Entry
		wantError bool
	}{
		{
			name: "type with nested tokens",
			raw:  `{"type":"codex","email":"user@example.com","tokens":{"access_token":"at-1","refresh_token":"rt-1"}}`,
			want: &Entry{Platform: "openai", Account: "user@example.com", AtToken: "at-1", RtToken: "rt-1"},
		},
		{
			name: "provider fallback",
			raw:  `{"provider":"xai","email":"user@example.com","access_token":"at-2"}`,
			want: &Entry{Platform: "grok", Account: "user@example.com", AtToken: "at-2"},
		},
		{
			name: "type wins over provider",
			raw:  `{"type":"claude","provider":"xai","email":"user@example.com","refresh_token":"rt-3"}`,
			want: &Entry{Platform: "anthropic", Account: "user@example.com", RtToken: "rt-3"},
		},
		{
			name: "valid type without credentials",
			raw:  `{"type":"codex","email":"user@example.com"}`,
		},
		{
			name:      "unsupported provider is an error",
			raw:       `{"type":"unknown","email":"user@example.com","access_token":"at"}`,
			wantError: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseCPAAuthFile([]byte(tt.raw))
			if tt.wantError {
				if err == nil {
					t.Fatal("parseCPAAuthFile error=nil, want error")
				}
				if !errors.Is(err, errUnsupportedCPAAuth) {
					t.Fatalf("parseCPAAuthFile error=%v, want unsupported format error", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseCPAAuthFile: %v", err)
			}
			if tt.want == nil {
				if got != nil {
					t.Fatalf("entry=%+v, want nil", got)
				}
				return
			}
			if got == nil || *got != *tt.want {
				t.Fatalf("entry=%+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestCPADirectReaderReadsRegularFilesAndRejectsSymlinks(t *testing.T) {
	root := t.TempDir()
	validPath := filepath.Join(root, "valid.json")
	if err := os.WriteFile(validPath, []byte(`{"type":"codex","email":"user@example.com","access_token":"at"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(root, "link.json")
	if err := os.Symlink(validPath, linkPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	reader, err := NewCPADirectReader(root)
	if err != nil {
		t.Fatal(err)
	}
	entry, _, err := reader.ReadEntry(context.Background(), validPath)
	if err != nil {
		t.Fatalf("ReadEntry: %v", err)
	}
	if entry == nil || entry.Account != "user@example.com" {
		t.Fatalf("entry=%+v, want supported regular file", entry)
	}
	if _, _, err := reader.ReadEntry(context.Background(), linkPath); err == nil {
		t.Fatal("symlink must be rejected")
	}
}

func TestCPADirectReaderRejectsRootSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := NewCPADirectReader(link); err == nil {
		t.Fatal("symlink root must be rejected")
	}
}

func TestCPADirectReaderEmitsFileChange(t *testing.T) {
	root := t.TempDir()
	reader, err := NewCPADirectReader(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := reader.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	path := filepath.Join(root, "auth.json")
	if err := os.WriteFile(path, []byte(`{"type":"codex","email":"user@example.com","access_token":"at","refresh_token":"rt"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case change := <-reader.Changes():
		if change.Path != path {
			t.Fatalf("change=%+v, want file change %q", change, path)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for CPA file change")
	}
}

func TestCPADirectReaderEmitsAtomicRenameChange(t *testing.T) {
	root := t.TempDir()
	reader, err := NewCPADirectReader(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := reader.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	tmpPath := filepath.Join(root, "account.json.tmp")
	path := filepath.Join(root, "account.json")
	if err := os.WriteFile(tmpPath, []byte(`{"type":"codex","email":"rename@example.com","access_token":"at","refresh_token":"rt"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		t.Fatal(err)
	}
	select {
	case change := <-reader.Changes():
		if change.Path != path {
			t.Fatalf("change=%+v, want renamed file %q", change, path)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for atomic rename change")
	}
}

func TestQueuePendingBoundsMemory(t *testing.T) {
	pending := make(map[string]time.Time, cpaDirectMaxPending)
	for i := 0; i < cpaDirectMaxPending; i++ {
		queuePending(pending, filepath.Join("/auth", string(rune(i+1))+".json"))
	}
	queuePending(pending, filepath.Join("/auth", "new.json"))
	if len(pending) != cpaDirectMaxPending {
		t.Fatalf("pending=%d, want cap %d", len(pending), cpaDirectMaxPending)
	}
	if _, ok := pending[filepath.Join("/auth", "new.json")]; !ok {
		t.Fatal("new path was not retained after queue reached capacity")
	}
}
