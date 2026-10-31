package main

import (
	"context"
	"fmt"
	"html"
	"io"
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

func (t *tescoProvider) Lookup(ctx context.Context, item string) (Price, error) {
	if err := t.wait(ctx); err != nil {
		return Price{}, err
	}
	u := t.baseURL + "/groceries/en-GB/search?query=" + url.QueryEscape(item)
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return Price{}, err
	}
	req.Header.Set("User-Agent", tescoUserAgent)
	req.Header.Set("Accept", "text/html")
	resp, err := t.client.Do(req)
	if err != nil {
		return Price{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK { // 403/429 = blocked: treated like any failure
		return Price{}, fmt.Errorf("tesco: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return Price{}, err
	}
	p, err := pickCheapest(item, parseTescoResults(string(body), t.baseURL))
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
