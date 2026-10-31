package main

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const tescoUserAgent = "houss-household-list/1.0 (personal shopping list; low volume, cached 12h)"

// tescoProvider scrapes Tesco product search results. Requests are serialised with a minimum gap.
type tescoProvider struct {
	baseURL string
	client  *http.Client
	gap     time.Duration

	mu   sync.Mutex
	last time.Time
}

func newTescoProvider() *tescoProvider {
	return &tescoProvider{
		baseURL: "https://www.tesco.com",
		client:  &http.Client{Timeout: 15 * time.Second},
		gap:     3 * time.Second,
	}
}

func (t *tescoProvider) Name() string { return "Tesco" }

func (t *tescoProvider) wait(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if d := time.Until(t.last.Add(t.gap)); d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	t.last = time.Now()
	return nil
}

// Diagnostic classifications for a Tesco response.
const (
	classOK         = "ok"
	classBlocked    = "blocked"     // 403/429 or a bot-challenge page
	classParseEmpty = "parse-empty" // 200 but zero product tiles
	classOther      = "other"
)

const tescoBrowserUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"

var challengeMarkers = []string{"access denied", "captcha", "pardon the interruption"}

// tescoDiag describes one Tesco response; it is the debug endpoint's JSON.
type tescoDiag struct {
	Profile        string            `json:"profile"`
	Status         int               `json:"status"`
	BodySize       int               `json:"body_size"`
	Snippet        string            `json:"snippet"`
	Headers        map[string]string `json:"headers"`
	CookieNames    []string          `json:"cookie_names"`
	Classification string            `json:"classification"`
	Tiles          int               `json:"tiles"`
	Error          string            `json:"error,omitempty"`
	prices         []Price
}

func classifyTesco(status int, body string, tiles int) string {
	if status == http.StatusForbidden || status == http.StatusTooManyRequests {
		return classBlocked
	}
	if status == http.StatusOK && tiles > 0 {
		return classOK // parsed products win over incidental marker words (e.g. a captcha script reference)
	}
	lower := strings.ToLower(body)
	for _, m := range challengeMarkers {
		if strings.Contains(lower, m) {
			return classBlocked
		}
	}
	if status != http.StatusOK {
		return classOther
	}
	return classParseEmpty
}

func snippet(body string, n int) string {
	r := []rune(body)
	if len(r) > n {
		r = r[:n]
	}
	return strings.Join(strings.Fields(string(r)), " ")
}

// fetch runs one search request; browser selects browser-like headers instead of the default identity.
func (t *tescoProvider) fetch(ctx context.Context, item string, browser bool) tescoDiag {
	d := tescoDiag{Profile: "default", Headers: map[string]string{}, CookieNames: []string{}, Classification: classOther}
	if browser {
		d.Profile = "browser"
	}
	u := t.baseURL + "/groceries/en-GB/search?query=" + url.QueryEscape(item)
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		d.Error = err.Error()
		return d
	}
	if browser {
		req.Header.Set("User-Agent", tescoBrowserUA)
		req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
		req.Header.Set("Accept-Language", "en-GB,en;q=0.9")
		req.Header.Set("Upgrade-Insecure-Requests", "1")
	} else {
		req.Header.Set("User-Agent", tescoUserAgent)
		req.Header.Set("Accept", "text/html")
	}
	resp, err := t.client.Do(req)
	if err != nil {
		d.Error = err.Error()
		return d
	}
	defer resp.Body.Close()
	d.Status = resp.StatusCode
	for _, h := range []string{"Server", "Cf-Ray", "Retry-After", "Content-Type"} {
		if v := resp.Header.Get(h); v != "" {
			d.Headers[h] = v
		}
	}
	for _, c := range resp.Cookies() {
		d.CookieNames = append(d.CookieNames, c.Name)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		d.Error = err.Error()
		return d
	}
	d.BodySize = len(body)
	d.Snippet = snippet(string(body), 300)
	d.prices = parseTescoResults(string(body), t.baseURL)
	d.Tiles = len(d.prices)
	d.Classification = classifyTesco(d.Status, string(body), d.Tiles)
	return d
}

// Diagnose runs one lookup per requested profile (default first) and returns each result.
func (t *tescoProvider) Diagnose(ctx context.Context, item string, compare bool) ([]tescoDiag, error) {
	profiles := []bool{false}
	if compare {
		profiles = append(profiles, true)
	}
	var out []tescoDiag
	for _, browser := range profiles {
		if err := t.wait(ctx); err != nil {
			return out, err
		}
		out = append(out, t.fetch(ctx, item, browser))
	}
	return out, nil
}

func (t *tescoProvider) Lookup(ctx context.Context, item string) (Price, error) {
	if err := t.wait(ctx); err != nil {
		return Price{}, err
	}
	d := t.fetch(ctx, item, false)
	snip := ""
	if d.Classification != classOK {
		snip = d.Snippet
	}
	log.Printf("tesco lookup %q: status=%d size=%d class=%s tiles=%d server=%q cf-ray=%q retry-after=%q cookies=%v snippet=%q err=%q",
		item, d.Status, d.BodySize, d.Classification, d.Tiles, d.Headers["Server"], d.Headers["Cf-Ray"],
		d.Headers["Retry-After"], d.CookieNames, snip, d.Error)
	addSpanAttr(ctx, strAttr("tesco.classification", d.Classification))
	if d.Error != "" {
		return Price{}, errors.New(d.Error)
	}
	if d.Classification != classOK { // blocked, parse-empty and other all fail like any lookup
		return Price{}, fmt.Errorf("tesco: HTTP %d (%s)", d.Status, d.Classification)
	}
	p, err := pickCheapest(item, d.prices)
	if err != nil {
		return Price{}, err
	}
	p.Store, p.FetchedAt = t.Name(), time.Now()
	return p, nil
}

var (
	tescoTileRe  = regexp.MustCompile(`(?s)<li\b[^>]*>(.*?)</li>`)
	tescoLinkRe  = regexp.MustCompile(`(?s)<a\b[^>]*href="(/groceries/en-GB/products/\d+)"[^>]*>(.*?)</a>`)
	tescoMoneyRe = regexp.MustCompile(`£\s*(\d+(?:\.\d+)?)(\s*/\s*[A-Za-z0-9]+)?`)
	tagRe        = regexp.MustCompile(`(?s)<[^>]*>`)
)

// parseTescoResults extracts product tiles (name, link, shelf price, unit price) from search HTML.
func parseTescoResults(page, base string) []Price {
	var out []Price
	for _, tile := range tescoTileRe.FindAllStringSubmatch(page, -1) {
		var name, href string
		for _, l := range tescoLinkRe.FindAllStringSubmatch(tile[1], -1) {
			if n := strings.TrimSpace(html.UnescapeString(tagRe.ReplaceAllString(l[2], " "))); n != "" {
				name, href = strings.Join(strings.Fields(n), " "), l[1]
				break
			}
		}
		if name == "" {
			continue
		}
		p := Price{Product: name, URL: base + href}
		text := html.UnescapeString(tagRe.ReplaceAllString(tile[1], " "))
		for _, m := range tescoMoneyRe.FindAllStringSubmatch(text, -1) {
			v, _ := strconv.ParseFloat(m[1], 64)
			if m[2] == "" && p.Price == 0 {
				p.Price = v
			} else if m[2] != "" && p.UnitPrice == 0 {
				p.UnitPrice = v
				p.UnitLabel = strings.TrimPrefix(strings.ReplaceAll(m[2], " ", ""), "/")
			}
		}
		if p.Price > 0 {
			out = append(out, p)
		}
	}
	return out
}
