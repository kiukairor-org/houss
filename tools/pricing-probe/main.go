// Command pricing-probe compares price sources and request options from the machine it runs on.
// It is a manual diagnostic: it is not part of the service and is never run by the cluster.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"text/tabwriter"
	"time"
)

// Same identities as the service's tesco.go; no other profiles are used for Tesco.
const (
	tescoHonestUA  = "houss-household-list/1.0 (personal shopping list; low volume, cached 12h)"
	tescoBrowserUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"
	probeUA        = "houss-pricing-probe/1.0 (manual homelab check; low volume)"
)

// Classifications recorded for each probe.
const (
	classOK      = "ok"      // 200 and at least one price parsed
	classBlocked = "blocked" // 403/429 or a bot-challenge page
	classEmpty   = "empty"   // 200 but no usable price
	classError   = "error"   // transport error or unexpected status
	classInfo    = "info"    // informational fetch (robots.txt)
)

var challengeMarkers = []string{"access denied", "captcha", "pardon the interruption"}

// Result is one request's outcome.
type Result struct {
	Source         string `json:"source"`
	Option         string `json:"option"`
	Item           string `json:"item"`
	URL            string `json:"url"`
	Status         int    `json:"status"`
	BodySize       int    `json:"body_size"`
	LatencyMS      int64  `json:"latency_ms"`
	PriceParsed    bool   `json:"price_parsed"`
	Prices         int    `json:"prices"`
	Classification string `json:"classification"`
	Note           string `json:"note,omitempty"`
	Error          string `json:"error,omitempty"`
}

// Report is the JSON file written at the end of a run.
type Report struct {
	StartedAt   time.Time `json:"started_at"`
	Items       []string  `json:"items"`
	Delay       string    `json:"delay"`
	MaxRequests int       `json:"max_requests"`
	Requests    int       `json:"requests"`
	Truncated   bool      `json:"truncated"`
	Results     []Result  `json:"results"`
}

// probe is one source/option; parse returns the number of prices found and a note.
type probe struct {
	source, option string
	buildURL       func(item string) string
	headers        map[string]string
	parse          func(body []byte) (int, string)
	info           bool // fetched once with no item and never classified as a price result
}

type config struct {
	items       []string
	delay       time.Duration
	maxRequests int
	timeout     time.Duration
	tescoURL    string
	offURL      string
	pricesURL   string
}

func classify(status int, body string, prices int) string {
	if status == http.StatusForbidden || status == http.StatusTooManyRequests {
		return classBlocked
	}
	if status == http.StatusOK && prices > 0 {
		return classOK // parsed prices win over incidental marker words
	}
	lower := strings.ToLower(body)
	for _, m := range challengeMarkers {
		if strings.Contains(lower, m) {
			return classBlocked
		}
	}
	if status == http.StatusOK {
		return classEmpty
	}
	return classError
}

func buildProbes(c config) []probe {
	tescoSearch := func(item string) string {
		return c.tescoURL + "/groceries/en-GB/search?query=" + url.QueryEscape(item)
	}
	return []probe{
		{source: "tesco", option: "honest-ua", buildURL: tescoSearch, parse: parseTesco,
			headers: map[string]string{"User-Agent": tescoHonestUA, "Accept": "text/html"}},
		{source: "tesco", option: "browser-headers", buildURL: tescoSearch, parse: parseTesco,
			headers: map[string]string{
				"User-Agent":                tescoBrowserUA,
				"Accept":                    "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8",
				"Accept-Language":           "en-GB,en;q=0.9",
				"Upgrade-Insecure-Requests": "1",
			}},
		{source: "tesco", option: "robots.txt", info: true,
			buildURL: func(string) string { return c.tescoURL + "/robots.txt" },
			headers:  map[string]string{"User-Agent": tescoHonestUA},
			parse:    func(b []byte) (int, string) { return 0, snippet(string(b), 200) }},
		{source: "open-food-facts", option: "search", parse: parseOFF,
			buildURL: func(item string) string {
				return c.offURL + "/cgi/search.pl?json=1&page_size=5&fields=code,product_name&search_terms=" + url.QueryEscape(item)
			},
			headers: map[string]string{"User-Agent": probeUA, "Accept": "application/json"}},
		{source: "open-prices", option: "prices-by-name", parse: parseOpenPrices,
			buildURL: func(item string) string {
				return c.pricesURL + "/api/v1/prices?size=5&product_name__like=" + url.QueryEscape(item)
			},
			headers: map[string]string{"User-Agent": probeUA, "Accept": "application/json"}},
	}
}

// run executes every probe for every item, pacing requests and stopping at the request cap.
func run(ctx context.Context, c config, client *http.Client) Report {
	rep := Report{StartedAt: time.Now().UTC(), Items: c.items, Delay: c.delay.String(), MaxRequests: c.maxRequests, Results: []Result{}}
	type job struct {
		p    probe
		item string
	}
	var jobs []job
	for _, p := range buildProbes(c) {
		if p.info {
			jobs = append(jobs, job{p, ""})
			continue
		}
		for _, it := range c.items {
			jobs = append(jobs, job{p, it})
		}
	}
	for _, j := range jobs {
		if rep.Requests >= c.maxRequests {
			rep.Truncated = true
			break
		}
		if rep.Requests > 0 {
			select {
			case <-time.After(c.delay):
			case <-ctx.Done():
				rep.Truncated = true
				return rep
			}
		}
		rep.Requests++
		rep.Results = append(rep.Results, fetch(ctx, client, j.p, j.item))
	}
	return rep
}

func fetch(ctx context.Context, client *http.Client, p probe, item string) Result {
	r := Result{Source: p.source, Option: p.option, Item: item, URL: p.buildURL(item), Classification: classError}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.URL, nil)
	if err != nil {
		r.Error = err.Error()
		return r
	}
	for k, v := range p.headers {
		req.Header.Set(k, v)
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		r.LatencyMS = time.Since(start).Milliseconds()
		r.Error = err.Error()
		return r
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	r.LatencyMS = time.Since(start).Milliseconds()
	r.Status, r.BodySize = resp.StatusCode, len(body)
	if err != nil {
		r.Error = err.Error()
		return r
	}
	r.Prices, r.Note = p.parse(body)
	r.PriceParsed = r.Prices > 0
	r.Classification = classify(r.Status, string(body), r.Prices)
	if p.info {
		if r.Classification == classEmpty {
			r.Classification = classInfo
		}
		r.PriceParsed = false
	}
	if r.Classification == classBlocked || r.Classification == classError {
		r.Note = snippet(string(body), 120)
	}
	return r
}

func snippet(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		r = r[:n]
	}
	return strings.Join(strings.Fields(string(r)), " ")
}

var (
	tileRe  = regexp.MustCompile(`(?s)<li\b[^>]*>(.*?)</li>`)
	linkRe  = regexp.MustCompile(`href="/groceries/en-GB/products/\d+"`)
	moneyRe = regexp.MustCompile(`£\s*\d+(?:\.\d+)?`)
	tagRe   = regexp.MustCompile(`(?s)<[^>]*>`)
)

// parseTesco counts product tiles (a product link plus a £ price) in search HTML.
func parseTesco(body []byte) (int, string) {
	n := 0
	for _, t := range tileRe.FindAllStringSubmatch(string(body), -1) {
		if linkRe.MatchString(t[1]) && moneyRe.MatchString(html.UnescapeString(tagRe.ReplaceAllString(t[1], " "))) {
			n++
		}
	}
	return n, fmt.Sprintf("%d product tiles", n)
}

// parseOFF reports how many products carry a price; Open Food Facts normally has none.
func parseOFF(body []byte) (int, string) {
	var v struct {
		Products []struct {
			Price any `json:"price"`
		} `json:"products"`
	}
	if json.Unmarshal(body, &v) != nil {
		return 0, "unparseable JSON"
	}
	priced := 0
	for _, p := range v.Products {
		if p.Price != nil {
			priced++
		}
	}
	return priced, fmt.Sprintf("%d products, %d with a price", len(v.Products), priced)
}

// parseOpenPrices counts community-submitted price records with a positive price.
func parseOpenPrices(body []byte) (int, string) {
	var v struct {
		Items []struct {
			Price float64 `json:"price"`
		} `json:"items"`
	}
	if json.Unmarshal(body, &v) != nil {
		return 0, "unparseable JSON"
	}
	n := 0
	for _, it := range v.Items {
		if it.Price > 0 {
			n++
		}
	}
	return n, fmt.Sprintf("%d price records", n)
}

// writeMatrix prints one row per source/option and one column per item.
func writeMatrix(w io.Writer, rep Report) {
	type key struct{ source, option string }
	var order []key
	cells := map[key]map[string]Result{}
	for _, r := range rep.Results {
		k := key{r.Source, r.Option}
		if cells[k] == nil {
			cells[k] = map[string]Result{}
			order = append(order, k)
		}
		cells[k][r.Item] = r
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprint(tw, "SOURCE/OPTION")
	for _, it := range rep.Items {
		fmt.Fprint(tw, "\t", it)
	}
	fmt.Fprintln(tw)
	for _, k := range order {
		fmt.Fprint(tw, k.source+"/"+k.option)
		if r, ok := cells[k][""]; ok { // informational, not per item
			fmt.Fprint(tw, "\t", cell(r))
		} else {
			for _, it := range rep.Items {
				if r, ok := cells[k][it]; ok {
					fmt.Fprint(tw, "\t", cell(r))
				} else {
					fmt.Fprint(tw, "\t-")
				}
			}
		}
		fmt.Fprintln(tw)
	}
	tw.Flush()
	fmt.Fprintf(w, "\n%d requests (cap %d)", rep.Requests, rep.MaxRequests)
	if rep.Truncated {
		fmt.Fprint(w, "; stopped early, some probes were not run")
	}
	fmt.Fprintln(w)
}

func cell(r Result) string {
	if r.Status == 0 {
		return r.Classification + " (no response)"
	}
	return fmt.Sprintf("%d %s %dB %dms", r.Status, r.Classification, r.BodySize, r.LatencyMS)
}

func writeReport(path string, rep Report) error {
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

func main() {
	var (
		items = flag.String("items", "milk,bread,eggs,butter,bananas", "comma-separated sample items")
		out   = flag.String("out", "pricing-probe-report.json", "path of the JSON report to write")
		c     config
	)
	flag.DurationVar(&c.delay, "delay", 3*time.Second, "pause between requests")
	flag.IntVar(&c.maxRequests, "max-requests", 30, "hard cap on total requests")
	flag.DurationVar(&c.timeout, "timeout", 15*time.Second, "per-request timeout")
	flag.StringVar(&c.tescoURL, "tesco-url", "https://www.tesco.com", "Tesco base URL")
	flag.StringVar(&c.offURL, "off-url", "https://world.openfoodfacts.org", "Open Food Facts base URL")
	flag.StringVar(&c.pricesURL, "prices-url", "https://prices.openfoodfacts.org", "Open Prices base URL")
	flag.Parse()
	for _, it := range strings.Split(*items, ",") {
		if it = strings.TrimSpace(it); it != "" {
			c.items = append(c.items, it)
		}
	}
	if len(c.items) == 0 || c.maxRequests < 1 {
		fmt.Fprintln(os.Stderr, "need at least one item and max-requests >= 1")
		os.Exit(2)
	}
	rep := run(context.Background(), c, &http.Client{Timeout: c.timeout})
	writeMatrix(os.Stdout, rep)
	if err := writeReport(*out, rep); err != nil {
		fmt.Fprintln(os.Stderr, "write report:", err)
		os.Exit(1)
	}
	fmt.Println("report written to", *out)
}
