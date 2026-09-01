package api

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReadJSONRejectsSecondValueBeyondLimit(t *testing.T) {
	body := `{"ok":true}` + strings.Repeat(" ", 1<<20) + `{"unexpected":true}`
	r := httptest.NewRequest("POST", "/", strings.NewReader(body))
	var dst map[string]bool
	if err := readJSON(r, &dst); err == nil {
		t.Fatal("readJSON accepted a second JSON value beyond its 1 MiB limit")
	}
}
