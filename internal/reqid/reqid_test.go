package reqid

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

func TestNewProducesDistinct32CharacterHexIDs(t *testing.T) {
	first, second := New(), New()
	if len(first) != 32 || len(second) != 32 {
		t.Fatalf("ID lengths = %d, %d; want 32", len(first), len(second))
	}
	if first == second {
		t.Fatal("random request IDs must differ")
	}
	for _, id := range []string{first, second} {
		for _, r := range id {
			if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
				t.Fatalf("ID %q is not lowercase hexadecimal", id)
			}
		}
	}
}

func TestHandlerAddsRequestIDOnlyForRequestContext(t *testing.T) {
	var out bytes.Buffer
	logger := slog.New(NewHandler(slog.NewJSONHandler(&out, nil)))
	logger.InfoContext(With(context.Background(), "request-123"), "request log")
	logger.Info("background log")

	logs := out.String()
	if !strings.Contains(logs, `"req_id":"request-123"`) {
		t.Fatalf("request log did not include req_id: %s", logs)
	}
	if strings.Count(logs, `"req_id"`) != 1 {
		t.Fatalf("background log must not have req_id: %s", logs)
	}
}
