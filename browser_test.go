package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBrowserSnippetInjectedIntoHead(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "snippet.html")
	if err := os.WriteFile(f, []byte(`<script type="text/javascript">window.NREUM={}</script>`), 0o600); err != nil {
		t.Fatal(err)
	}
	prev := browserAgent
	browserAgent = loadBrowserSnippet(f)
	t.Cleanup(func() { browserAgent = prev })
	rec := httptest.NewRecorder()
	newMux(testServer(t)).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	page := rec.Body.String()
	nr, head, app := strings.Index(page, "window.NREUM"), strings.Index(page, "</head>"), strings.Index(page, `src="/app.js"`)
	if nr < 0 || nr > head || nr > app || !strings.HasPrefix(page[strings.Index(page, "<meta charset"):], `<meta charset="utf-8">`+"\n<script") {
		t.Fatalf("snippet must open <head>, right after <meta charset>:\n%s", page[:400])
	}
}

func TestNoBrowserSnippetWithoutFile(t *testing.T) {
	if loadBrowserSnippet("") != nil || loadBrowserSnippet("/nonexistent") != nil {
		t.Fatal("no file must mean no snippet")
	}
	if got := withBrowserAgent([]byte("<head>\n</head>"), nil); string(got) != "<head>\n</head>" {
		t.Fatalf("page changed without a snippet: %q", got)
	}
}
