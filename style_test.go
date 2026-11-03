package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The look is driven by tokens defined once in :root; light only, no hard-coded colours elsewhere.
func TestStyleTokens(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("web", "style.css"))
	if err != nil {
		t.Fatal(err)
	}
	css := string(b)
	start := strings.Index(css, ":root")
	end := strings.Index(css, "\n}")
	if start < 0 || end < start {
		t.Fatal("no :root block")
	}
	root, rest := css[start:end], css[end:]
	for _, v := range []string{"--accent:", "--card:", "--line:", "--glow:", "--success:", "--warning:", "--error:", "--r:", "--r-pill:", "--shadow-1:", "--shadow-2:", "--shadow-3:", "--shadow-4:", "--shadow-5:"} {
		if !strings.Contains(root, v) {
			t.Errorf(":root is missing token %s", v)
		}
	}
	if strings.Contains(css, "prefers-color-scheme") || strings.Contains(css, "light dark") {
		t.Error("style.css must be light only")
	}
	if m := regexp.MustCompile(`:\s*#[0-9a-fA-F]{3,8}\b|[\s(,]#[0-9a-fA-F]{3,8}[;\s,)]|rgba?\(|url\(|@import`).FindString(rest); m != "" {
		t.Errorf("colour or external resource %q outside :root; use a CSS variable", m)
	}
	if !strings.Contains(rest, "var(--r-pill)") {
		t.Error("pill radius token unused")
	}
}
