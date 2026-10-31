package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	for _, p := range []string{"/app.js", "/style.css", "/sw.js", "/manifest.webmanifest", "/icons/192.png", "/icons/512.png", "/icons/180.png"} {
		if rec := do(t, h, "GET", p, "", nil); rec.Code != 200 || rec.Body.Len() == 0 {
			t.Fatalf("%s: %d", p, rec.Code)
		}
	}
	if rec := do(t, h, "GET", "/icons/999.png", "", nil); rec.Code != 404 {
		t.Fatalf("got %d", rec.Code)
	}
	var m struct{ Icons []struct{ Sizes string } }
	_ = json.Unmarshal(do(t, h, "GET", "/manifest.webmanifest", "", nil).Body.Bytes(), &m)
	if len(m.Icons) < 2 {
		t.Fatalf("manifest icons: %v", m)
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
