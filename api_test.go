package main

import (
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

const testPassword = "correct horse"

func testServer(t *testing.T) *server {
	t.Helper()
	st, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &server{auth: newAuth(testPassword, ""), store: st}
}

func do(t *testing.T, h http.Handler, method, path, body string, c *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if c != nil {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func login(t *testing.T, h http.Handler) *http.Cookie {
	t.Helper()
	rec := do(t, h, "POST", "/api/login", `{"password":"`+testPassword+`"}`, nil)
	if rec.Code != 200 {
		t.Fatalf("login: %d", rec.Code)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == cookieName {
			if !c.HttpOnly || !c.Secure || c.MaxAge < 24*3600 {
				t.Fatalf("weak cookie: %+v", c)
			}
			return c
		}
	}
	t.Fatal("no session cookie")
	return nil
}

func listItems(t *testing.T, h http.Handler, path string, c *http.Cookie) []Item {
	t.Helper()
	var list []Item
	if err := json.Unmarshal(do(t, h, "GET", path, "", c).Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	return list
}

func TestAuth(t *testing.T) {
	h := newMux(testServer(t))
	if rec := do(t, h, "GET", "/api/items", "", nil); rec.Code != 401 {
		t.Fatalf("no cookie: %d", rec.Code)
	}
	if rec := do(t, h, "POST", "/api/login", `{"password":"nope"}`, nil); rec.Code != 401 || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("wrong password: %d", rec.Code)
	}
	if rec := do(t, h, "GET", "/api/items", "", &http.Cookie{Name: cookieName, Value: "9999999999.bad"}); rec.Code != 401 {
		t.Fatalf("forged cookie: %d", rec.Code)
	}
	c := login(t, h)
	if rec := do(t, h, "GET", "/api/items", "", c); rec.Code != 200 {
		t.Fatalf("with cookie: %d", rec.Code)
	}
}

func TestSessionExpires(t *testing.T) {
	s := testServer(t)
	h := newMux(s)
	c := login(t, h)
	s.auth.now = func() time.Time { return time.Now().Add(sessionTTL + time.Hour) }
	if rec := do(t, h, "GET", "/api/items", "", c); rec.Code != 401 {
		t.Fatalf("expired: %d", rec.Code)
	}
}

func TestLoginRateLimit(t *testing.T) {
	h := newMux(testServer(t))
	for i := 0; i < maxLoginFails; i++ {
		do(t, h, "POST", "/api/login", `{"password":"x"}`, nil)
	}
	// Even the right password is refused while limited.
	if rec := do(t, h, "POST", "/api/login", `{"password":"`+testPassword+`"}`, nil); rec.Code != 429 {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestCRUDEachTab(t *testing.T) {
	h := newMux(testServer(t))
	c := login(t, h)
	for tab := range tabs {
		rec := do(t, h, "POST", "/api/items", `{"title":"a `+tab+`","tab":"`+tab+`","target_date":"2030-01-02"}`, c)
		if rec.Code != 201 {
			t.Fatalf("%s create: %d %s", tab, rec.Code, rec.Body)
		}
		var it Item
		_ = json.Unmarshal(rec.Body.Bytes(), &it)
		rec = do(t, h, "PUT", "/api/items/"+it.ID, `{"title":"edited","tab":"`+tab+`","done":true}`, c)
		var got Item
		_ = json.Unmarshal(rec.Body.Bytes(), &got)
		if rec.Code != 200 || got.Title != "edited" || !got.Done || got.TargetDate != "" {
			t.Fatalf("%s edit: %d %+v", tab, rec.Code, got)
		}
		if n := len(listItems(t, h, "/api/items?tab="+tab, c)); n != 1 {
			t.Fatalf("%s list: %d", tab, n)
		}
		if rec := do(t, h, "DELETE", "/api/items/"+it.ID, "", c); rec.Code != 200 {
			t.Fatalf("%s delete: %d", tab, rec.Code)
		}
		if n := len(listItems(t, h, "/api/items?tab="+tab, c)); n != 0 {
			t.Fatalf("%s not deleted: %d", tab, n)
		}
	}
}

func TestValidation(t *testing.T) {
	h := newMux(testServer(t))
	c := login(t, h)
	for _, b := range []string{`{"title":"","tab":"todos"}`, `{"title":"x","tab":"bogus"}`, `{"title":"x","tab":"todos","target_date":"tomorrow"}`, `nope`} {
		if rec := do(t, h, "POST", "/api/items", b, c); rec.Code != 400 {
			t.Fatalf("%s: %d", b, rec.Code)
		}
	}
	if rec := do(t, h, "DELETE", "/api/items/missing", "", c); rec.Code != 404 {
		t.Fatalf("delete missing: %d", rec.Code)
	}
}

func TestAllViewAndPersistence(t *testing.T) {
	dir := t.TempDir()
	st, _ := openStore(dir)
	h := newMux(&server{auth: newAuth(testPassword, ""), store: st})
	c := login(t, h)
	for tab := range tabs {
		do(t, h, "POST", "/api/items", `{"title":"x","tab":"`+tab+`"}`, c)
	}
	seen := map[string]bool{}
	list := listItems(t, h, "/api/items", c)
	for _, it := range list {
		seen[it.Tab] = true
	}
	if len(list) != 3 || len(seen) != 3 {
		t.Fatalf("all view: %v", list)
	}
	// "Restart": reopen the store; the old cookie still works (stateless sessions).
	st2, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	h2 := newMux(&server{auth: newAuth(testPassword, ""), store: st2})
	if n := len(listItems(t, h2, "/api/items", c)); n != 3 {
		t.Fatalf("after restart: %d", n)
	}
}

func TestSyncLastWriteWins(t *testing.T) {
	h := newMux(testServer(t))
	c := login(t, h)
	body := `{"changes":[
	 {"id":"a","title":"new","tab":"todos","updated_at":200},
	 {"id":"a","title":"stale","tab":"todos","updated_at":100},
	 {"id":"b","title":"gone","tab":"shopping","updated_at":50},
	 {"id":"b","title":"gone","tab":"shopping","updated_at":60,"deleted":true}]}`
	rec := do(t, h, "POST", "/api/sync", body, c)
	var out struct{ Items []Item }
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != 200 || len(out.Items) != 1 || out.Items[0].Title != "new" {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	// An older edit replayed after a newer delete must not resurrect the item.
	do(t, h, "POST", "/api/sync", `{"changes":[{"id":"b","title":"late","tab":"shopping","updated_at":55}]}`, c)
	if n := len(listItems(t, h, "/api/items", c)); n != 1 {
		t.Fatalf("resurrected: %d", n)
	}
}

func TestAppShellPublic(t *testing.T) {
	h := newMux(testServer(t))
	for _, p := range []string{"/app.js", "/style.css", "/sw.js", "/manifest.webmanifest", "/icons/icon.svg", "/icons/favicon-32.png", "/icons/apple-touch-180.png", "/icons/icon-192.png", "/icons/icon-512.png", "/icons/icon-maskable-512.png"} {
		if rec := do(t, h, "GET", p, "", nil); rec.Code != 200 || rec.Body.Len() == 0 {
			t.Fatalf("%s: %d", p, rec.Code)
		}
	}
	// the previous house icons are gone
	for _, p := range []string{"/icons/999.png", "/icons/192.png", "/icons/512.png", "/icons/180.png", "/icons/maskable-512.png"} {
		if rec := do(t, h, "GET", p, "", nil); rec.Code != 404 {
			t.Fatalf("%s: got %d", p, rec.Code)
		}
	}
	var m struct{ Icons []struct{ Sizes string } }
	_ = json.Unmarshal(do(t, h, "GET", "/manifest.webmanifest", "", nil).Body.Bytes(), &m)
	if len(m.Icons) < 2 {
		t.Fatalf("manifest icons: %v", m)
	}
}

func TestIconsArePurpleAndMaskableIsFullBleed(t *testing.T) {
	h := newMux(testServer(t))
	decode := func(p string) image.Image {
		img, err := png.Decode(do(t, h, "GET", p, "", nil).Body)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		return img
	}
	for _, p := range []string{"/icons/favicon-32.png", "/icons/apple-touch-180.png", "/icons/icon-192.png", "/icons/icon-512.png", "/icons/icon-maskable-512.png"} {
		img := decode(p)
		b := img.Bounds()
		r, g, bl, _ := img.At(b.Dx()/2, b.Dy()*95/100).RGBA() // background below the mark
		if g >= r || g >= bl {
			t.Fatalf("%s background not purple: %d %d %d", p, r, g, bl)
		}
	}
	if _, _, _, a := decode("/icons/icon-maskable-512.png").At(0, 0).RGBA(); a != 0xffff {
		t.Fatal("maskable icon must be full-bleed")
	}
	if _, _, _, a := decode("/icons/icon-512.png").At(0, 0).RGBA(); a != 0 {
		t.Fatal("regular icon should have transparent rounded corners")
	}
	// the manifest and the HTML head reference only icons that are served (with 200), incl. maskable
	var m struct {
		Icons []struct{ Src, Sizes, Purpose string }
	}
	_ = json.Unmarshal(do(t, h, "GET", "/manifest.webmanifest", "", nil).Body.Bytes(), &m)
	sizes, maskable := map[string]bool{}, false
	for _, ic := range m.Icons {
		sizes[ic.Sizes] = true
		maskable = maskable || ic.Purpose == "maskable"
		if rec := do(t, h, "GET", ic.Src, "", nil); rec.Code != 200 {
			t.Errorf("manifest icon %s: %d", ic.Src, rec.Code)
		}
	}
	if !sizes["192x192"] || !sizes["512x512"] || !maskable {
		t.Fatalf("manifest needs 192, 512 and a maskable icon: %v", m)
	}
	index := do(t, h, "GET", "/", "", nil).Body.String()
	refs := regexp.MustCompile(`<link rel="(?:icon|apple-touch-icon)"[^>]*href="([^"]+)"`).FindAllStringSubmatch(index, -1)
	if len(refs) < 2 {
		t.Fatalf("index.html icon links: %v", refs)
	}
	for _, r := range refs {
		if rec := do(t, h, "GET", r[1], "", nil); rec.Code != 200 {
			t.Errorf("head icon %s: %d", r[1], rec.Code)
		}
	}
	for _, old := range []string{"/icons/192.png", "/icons/512.png", "/icons/180.png", "/icons/maskable-512.png"} {
		for name, s := range map[string]string{"index": index, "manifest": do(t, h, "GET", "/manifest.webmanifest", "", nil).Body.String()} {
			if strings.Contains(s, old) {
				t.Errorf("%s still references old icon %s", name, old)
			}
		}
	}
	if sw := do(t, h, "GET", "/sw.js", "", nil).Body.String(); strings.Contains(sw, "shell-v6") {
		t.Fatal("service worker cache version not bumped")
	}
}

// Every pixel of the icons and every colour in the SVG comes from the app's CSS palette.
func TestIconUsesOnlyPalette(t *testing.T) {
	css, err := os.ReadFile(filepath.Join("web", "style.css"))
	if err != nil {
		t.Fatal(err)
	}
	hex := regexp.MustCompile(`#[0-9a-fA-F]{6}\b`)
	palette := map[string]bool{}
	for _, c := range hex.FindAllString(string(css), -1) {
		palette[strings.ToLower(c)] = true
	}
	svg, err := os.ReadFile(filepath.Join("web", "icons", "icon.svg"))
	if err != nil {
		t.Fatal(err)
	}
	cols := hex.FindAllString(string(svg), -1)
	if len(cols) == 0 {
		t.Fatal("icon.svg has no colours")
	}
	for _, c := range cols {
		if !palette[strings.ToLower(c)] {
			t.Errorf("icon.svg colour %s is not in style.css", c)
		}
	}
	// the PNG renderer draws with these colours (anti-aliased edges only blend between them)
	for _, c := range []color.NRGBA{iconDeep, iconMid, iconEdge, iconLight} {
		if !palette[fmt.Sprintf("#%02x%02x%02x", c.R, c.G, c.B)] {
			t.Errorf("renderer colour %v is not in style.css", c)
		}
	}
}

func TestIndexHasRedesignMarkup(t *testing.T) {
	h := newMux(testServer(t))
	body := do(t, h, "GET", "/", "", nil).Body.String()
	for _, want := range []string{`id="empty"`, `class="count"`, `<label>Due`} {
		if !strings.Contains(body, want) {
			t.Fatalf("index.html missing %s", want)
		}
	}
}

func TestIndexTabOrder(t *testing.T) {
	h := newMux(testServer(t))
	body := do(t, h, "GET", "/", "", nil).Body.String()
	last := -1
	for _, tab := range []string{"shopping", "todos", "midterm", "all"} {
		i := strings.Index(body, `data-tab="`+tab+`"`)
		if i <= last {
			t.Fatalf("tab %s out of order", tab)
		}
		last = i
	}
	if !strings.Contains(body, `data-tab="shopping" class="active"`) {
		t.Fatal("shopping should be the default tab")
	}
}

func TestIndexHasNoListDropdown(t *testing.T) {
	h := newMux(testServer(t))
	body := do(t, h, "GET", "/", "", nil).Body.String()
	for _, bad := range []string{`<label>List`, `id="add-tab"`} {
		if strings.Contains(body, bad) {
			t.Fatalf("index.html still contains %s", bad)
		}
	}
	js := do(t, h, "GET", "/app.js", "", nil).Body.String()
	if strings.Contains(js, "add-tab") {
		t.Fatal("app.js still references add-tab")
	}
}
