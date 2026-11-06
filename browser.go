// New Relic Browser (RUM, D81): the copy/paste snippet comes from the file named by $NEW_RELIC_BROWSER_SNIPPET_FILE
// (a ConfigMap in deploy/, optional) and is injected at the top of index.html's <head>. No file, no RUM: nothing
// account-specific lives in the code. With distributed tracing on in the snippet, the browser adds traceparent to
// same-origin API calls, so a tap in the UI and its server span share one trace.
package main

import (
	"bytes"
	"log"
	"os"
)

// browserAgent is the snippet injected into index.html (empty = RUM off).
var browserAgent = loadBrowserSnippet(os.Getenv("NEW_RELIC_BROWSER_SNIPPET_FILE"))

func loadBrowserSnippet(path string) []byte {
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil || !bytes.HasPrefix(bytes.TrimSpace(b), []byte("<script")) {
		log.Printf("browser monitoring off: no snippet at %s (%v)", path, err)
		return nil
	}
	return append(bytes.TrimSpace(b), '\n')
}

// withBrowserAgent puts the snippet right after <meta charset> (or <head>), before any other script.
func withBrowserAgent(page, snippet []byte) []byte {
	if len(snippet) == 0 {
		return page
	}
	for _, anchor := range [][]byte{[]byte("<meta charset=\"utf-8\">\n"), []byte("<head>\n")} {
		if i := bytes.Index(page, anchor); i >= 0 {
			at := i + len(anchor)
			out := make([]byte, 0, len(page)+len(snippet))
			return append(append(append(out, page[:at]...), snippet...), page[at:]...)
		}
	}
	return page
}
