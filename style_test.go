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

// The accent is the purple #35063E with white text on it, and no mint/green accent remains.
func TestStylePurpleTheme(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("web", "style.css"))
	if err != nil {
		t.Fatal(err)
	}
	css := strings.ToLower(string(b))
	if !strings.Contains(css, "--purple-900: #35063e") || !strings.Contains(css, "--accent: var(--purple-900)") {
		t.Error("accent must be #35063E")
	}
	if !strings.Contains(css, "--on-accent: #fff;") {
		t.Error("text on the accent must be white")
	}
	if strings.Contains(css, "mint") || strings.Contains(css, "60 199 154") {
		t.Error("mint accent remains in style.css")
	}
	for _, f := range []string{"index.html", "manifest.webmanifest"} {
		h, err := os.ReadFile(filepath.Join("web", f))
		if err != nil {
			t.Fatal(err)
		}
		s := strings.ToLower(string(h))
		if strings.Contains(s, "3cc79a") || !strings.Contains(s, "#35063e") {
			t.Errorf("%s theme colour must be #35063E", f)
		}
	}
}
