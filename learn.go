// Learning buying patterns from the purchase history (history.go). Code does all the counting and
// date arithmetic (a 1.7B model is poor at it); the shared LLM only groups names that mean the same
// product, writes a short profile of the household's habits each morning, and picks which overdue
// items to suggest. Suggestions are shown, never added on their own. With the LLM off, down or
// asleep, plain statistics still give suggestions.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// learnTimeout bounds each background LLM call (a var for tests).
var learnTimeout = 60 * time.Second

const (
	learnEvery     = time.Minute
	suggestMaxAge  = 3 * time.Hour
	retryAfter     = 15 * time.Minute
	maxSuggestions = 5
	maxCandidates  = 15
	profileLines   = 12
	profileLineMax = 200
	groupTitlesMax = 150
	minDismissDays = 3
)

type dismissal struct {
	Name string `json:"name"`
	At   int64  `json:"at"` // unix ms
}

// learnState is what persists in learn.json.
type learnState struct {
	// Groups maps a normalised title to its group's display name, always one of the titles.
	Groups     map[string]string `json:"groups"`
	Profile    string            `json:"profile"`
	ProfileAt  int64             `json:"profile_at"`
	ProfileDay string            `json:"profile_day"` // London date of the last morning update
	Edited     bool              `json:"edited"`      // last written by the person, not the LLM
	// Pinned are the lines the person wrote themselves; every learning run keeps them.
	Pinned    []string    `json:"pinned,omitempty"`
	Dismissed []dismissal `json:"dismissed"`
}

type Suggestion struct {
	Name   string `json:"name"`
	Reason string `json:"reason"` // from code statistics, never from the LLM
}

type learner struct {
	store *store
	llm   *llmClient
	path  string
	kick  chan bool // true = refresh now (the profile too), outside the morning window

	mu      sync.Mutex
	st      learnState
	picks   []Suggestion // the LLM's last picks, nil until it has answered
	picksAt time.Time
	picksRv int64 // store revision the picks were made for
	tried   map[string]time.Time
}

func newLearner(dir string, st *store, l *llmClient) *learner {
	ln := &learner{store: st, llm: l, kick: make(chan bool, 1), st: learnState{Groups: map[string]string{}}, tried: map[string]time.Time{}}
	if dir == "" {
		return ln
	}
	ln.path = filepath.Join(dir, "learn.json")
	if b, err := os.ReadFile(ln.path); err == nil {
		if err := json.Unmarshal(b, &ln.st); err != nil {
			log.Printf("learn: ignoring unreadable %s: %v", ln.path, err)
		}
	}
	if ln.st.Groups == nil {
		ln.st.Groups = map[string]string{}
	}
	return ln
}

func (ln *learner) saveLocked() {
	if ln.path == "" {
		return
	}
	b, err := json.Marshal(ln.st)
	if err == nil {
		tmp := ln.path + ".tmp"
		if err = os.WriteFile(tmp, b, 0o644); err == nil {
			err = os.Rename(tmp, ln.path)
		}
	}
	if err != nil {
		log.Printf("learn: save: %v", err)
	}
}

var punct = regexp.MustCompile(`[^\p{L}\p{N} ]+`)

func normTitle(s string) string {
	return strings.Join(strings.Fields(punct.ReplaceAllString(strings.ToLower(s), " ")), " ")
}

// ---- statistics (code only) ----

type groupStat struct {
	Name       string
	Key        string
	Count      int     // days bought (several ticks the same day count once)
	Median     float64 // days between purchases
	Last       string  // London date
	DaysSince  int
	Ratio      float64 // DaysSince / Median
	Companions []string
}

func londonDay(ms int64) string { return time.UnixMilli(ms).In(london).Format("2006-01-02") }

func daysBetween(a, b string) int {
	ta, _ := time.Parse("2006-01-02", a)
	tb, _ := time.Parse("2006-01-02", b)
	return int(math.Round(tb.Sub(ta).Hours() / 24))
}

func (ln *learner) nameLocked(title string) (key, name string) {
	k := normTitle(title)
	if n, ok := ln.st.Groups[k]; ok {
		return normTitle(n), n
	}
	return k, strings.TrimSpace(title)
}

// statsLocked groups the history by product and works out each group's rhythm as of now.
func (ln *learner) statsLocked(hist []Purchase, now time.Time) map[string]*groupStat {
	today := now.In(london).Format("2006-01-02")
	days := map[string]map[string]bool{}  // group key -> days
	byDay := map[string]map[string]bool{} // day -> group keys
	out := map[string]*groupStat{}
	for i := len(hist) - 1; i >= 0; i-- { // oldest first, so the newest spelling names the group
		k, n := ln.nameLocked(hist[i].Title)
		if out[k] == nil {
			out[k] = &groupStat{Key: k}
			days[k] = map[string]bool{}
		}
		out[k].Name = n
		d := londonDay(hist[i].BoughtAt)
		days[k][d] = true
		if byDay[d] == nil {
			byDay[d] = map[string]bool{}
		}
		byDay[d][k] = true
	}
	for k, g := range out {
		ds := make([]string, 0, len(days[k]))
		for d := range days[k] {
			ds = append(ds, d)
		}
		sort.Strings(ds)
		g.Count, g.Last = len(ds), ds[len(ds)-1]
		g.DaysSince = daysBetween(g.Last, today)
		var gaps []int
		for i := 1; i < len(ds); i++ {
			gaps = append(gaps, daysBetween(ds[i-1], ds[i]))
		}
		if len(gaps) > 0 {
			sort.Ints(gaps)
			m := len(gaps) / 2
			g.Median = float64(gaps[m])
			if len(gaps)%2 == 0 {
				g.Median = float64(gaps[m-1]+gaps[m]) / 2
			}
		}
		if g.Median > 0 {
			g.Ratio = float64(g.DaysSince) / g.Median
		}
	}
	// Bought together on at least two days.
	pairs := map[[2]string]int{}
	for _, ks := range byDay {
		for a := range ks {
			for b := range ks {
				if a < b {
					pairs[[2]string{a, b}]++
				}
			}
		}
	}
	for p, n := range pairs {
		if n >= 2 {
			out[p[0]].Companions = append(out[p[0]].Companions, out[p[1]].Name)
			out[p[1]].Companions = append(out[p[1]].Companions, out[p[0]].Name)
		}
	}
	for _, g := range out {
		sort.Strings(g.Companions)
	}
	return out
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

func (g *groupStat) reason() string {
	every := int(math.Round(g.Median))
	since := "today"
	if g.DaysSince > 0 {
		since = plural(g.DaysSince, "day", "days") + " ago"
	}
	if every == 1 {
		return "usually every day, last bought " + since
	}
	return fmt.Sprintf("usually every %d days, last bought %s", every, since)
}

// candidatesLocked: products bought at least twice, not on the Shopping list right now, not
// recently dismissed, and due again (or nearly, when bought with something already listed).
func (ln *learner) candidatesLocked(stats map[string]*groupStat, items []Item, now time.Time) []*groupStat {
	open := map[string]bool{}
	for _, it := range items {
		if it.Tab == "shopping" && !it.Done {
			k, _ := ln.nameLocked(it.Title)
			open[k] = true
		}
	}
	var out []*groupStat
	for k, g := range stats {
		if g.Count < 2 || g.Median < 1 || open[k] || ln.dismissedLocked(g, now) {
			continue
		}
		near := false
		for _, c := range g.Companions {
			if open[normTitle(c)] {
				near = true
			}
		}
		if g.Ratio >= 0.8 || (near && g.Ratio >= 0.5) {
			out = append(out, g)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Ratio != out[j].Ratio {
			return out[i].Ratio > out[j].Ratio
		}
		return out[i].Name < out[j].Name
	})
	if len(out) > maxCandidates {
		out = out[:maxCandidates]
	}
	return out
}

// A dismissed product stays hidden until it is bought again, for at least its usual gap.
func (ln *learner) dismissedLocked(g *groupStat, now time.Time) bool {
	hide := time.Duration(max(g.Median, minDismissDays)*24) * time.Hour
	for _, d := range ln.st.Dismissed {
		if normTitle(d.Name) == g.Key && londonDay(d.At) >= g.Last && now.Sub(time.UnixMilli(d.At)) < hide {
			return true
		}
	}
	return false
}

// Suggestions returns what to show above the Shopping list and where it came from: "llm" (its
// picks, re-checked against the list as it is now) or "stats" (most overdue first).
func (ln *learner) Suggestions(now time.Time) (string, []Suggestion) {
	hist, items := ln.store.History(0, historyMax), ln.store.List("")
	ln.mu.Lock()
	defer ln.mu.Unlock()
	cands := ln.candidatesLocked(ln.statsLocked(hist, now), items, now)
	byKey := map[string]*groupStat{}
	for _, c := range cands {
		byKey[c.Key] = c
	}
	out := []Suggestion{}
	if ln.picks != nil && ln.llm.Available() {
		for _, p := range ln.picks {
			if c := byKey[normTitle(p.Name)]; c != nil {
				out = append(out, Suggestion{Name: c.Name, Reason: c.reason()})
			}
		}
		return "llm", out
	}
	for _, c := range cands {
		if c.Ratio >= 1 && len(out) < maxSuggestions {
			out = append(out, Suggestion{Name: c.Name, Reason: c.reason()})
		}
	}
	return "stats", out
}

func (ln *learner) Dismiss(name string, now time.Time) {
	ln.mu.Lock()
	defer ln.mu.Unlock()
	ln.st.Dismissed = append(ln.st.Dismissed, dismissal{Name: name, At: now.UnixMilli()})
	if n := len(ln.st.Dismissed); n > 500 {
		ln.st.Dismissed = ln.st.Dismissed[n-500:]
	}
	kept := ln.picks[:0:0]
	for _, p := range ln.picks {
		if normTitle(p.Name) != normTitle(name) {
			kept = append(kept, p)
		}
	}
	if ln.picks != nil {
		ln.picks = kept
	}
	ln.saveLocked()
}

// ---- profile ----

func (ln *learner) Profile() (string, int64, bool) {
	ln.mu.Lock()
	defer ln.mu.Unlock()
	return ln.st.Profile, ln.st.ProfileAt, ln.st.Edited
}

// cleanProfile trims a profile to profileLines lines of at most profileLineMax characters.
func cleanProfile(lines []string) ([]string, error) {
	var out []string
	for _, l := range lines {
		for _, s := range strings.Split(l, "\n") {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
	}
	if len(out) > profileLines {
		return nil, errInvalid
	}
	for _, s := range out {
		if len([]rune(s)) > profileLineMax {
			return nil, errInvalid
		}
	}
	return out, nil
}

// SetProfile stores the person's own version; an empty one clears what houss knows. Lines that
// houss did not write itself are pinned, so the next learning run keeps them.
func (ln *learner) SetProfile(text string, now time.Time) error {
	lines, err := cleanProfile([]string{text})
	if err != nil {
		return err
	}
	ln.mu.Lock()
	defer ln.mu.Unlock()
	learned := map[string]bool{}
	for _, l := range strings.Split(ln.st.Profile, "\n") {
		learned[normTitle(l)] = true
	}
	for _, l := range ln.st.Pinned {
		learned[normTitle(l)] = false
	}
	ln.st.Pinned = nil
	for _, l := range lines {
		if !learned[normTitle(l)] {
			ln.st.Pinned = append(ln.st.Pinned, l)
		}
	}
	ln.st.Profile, ln.st.ProfileAt, ln.st.Edited = strings.Join(lines, "\n"), now.UnixMilli(), len(lines) > 0
	ln.saveLocked()
	return nil
}

// chatContext is added to the assistant's system prompt.
func (ln *learner) chatContext() string {
	p, _, _ := ln.Profile()
	if p == "" {
		return ""
	}
	return "\nThe household's shopping habits, learned from what they ticked as bought (use these to answer " +
		"questions about what they usually buy and how often; quote them, do not list items for that):\n" + p + "\n"
}

// ---- LLM steps ----

var groupSchema = map[string]any{"type": "object", "required": []string{"groups"},
	"properties": map[string]any{"groups": map[string]any{"type": "array", "items": map[string]any{
		"type": "object", "required": []string{"name", "titles"},
		"properties": map[string]any{
			"name":   map[string]any{"type": "string"},
			"titles": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		}}}}}

const groupSystem = "You group shopping list names that mean the same product: case, singular/plural, French and " +
	"English names, small variants (\"semi-skimmed milk\" and \"milk\"). Different products stay apart (\"milk\" and " +
	"\"oat milk\" are different). Only list groups of two or more names. Copy names exactly from the list; never " +
	"add a name. name is the group's simplest name, taken from its titles. Answer with compact one-line JSON."

// regroup asks the LLM which titles are the same product. An invented name rejects the whole
// answer; a name put in two groups stays in the first; names it leaves out stay on their own.
func (ln *learner) regroup(ctx context.Context, titles []string) (map[string]string, error) {
	var out struct {
		Groups []struct {
			Name   string   `json:"name"`
			Titles []string `json:"titles"`
		} `json:"groups"`
	}
	msgs := []llmMsg{{"system", groupSystem}, {"user", "Names:\n" + strings.Join(titles, "\n")}}
	if err := ln.llm.chat(ctx, msgs, groupSchema, &out); err != nil {
		return nil, err
	}
	known := map[string]string{} // norm -> as written in the list
	for _, t := range titles {   // the first spelling (the newest) names the group
		if k := normTitle(t); known[k] == "" {
			known[k] = t
		}
	}
	m := map[string]string{}
	for _, g := range out.Groups {
		// The model often names a group by one of the list's names without repeating it in titles.
		var members []string
		for n, t := range append([]string{g.Name}, g.Titles...) {
			k := normTitle(t)
			if _, ok := known[k]; !ok {
				if n == 0 { // a made-up group name is fine; it just is not a member
					continue
				}
				return nil, fmt.Errorf("%w: invented name %q", errLLMOutput, t)
			}
			if _, taken := m[k]; taken { // in two groups: the first one keeps it
				continue
			}
			m[k] = ""
			members = append(members, k)
		}
		if len(members) == 0 {
			continue
		}
		name := ""
		if k := normTitle(g.Name); m[k] == "" && slices.Contains(members, k) {
			name = known[k]
		} else { // not one of its titles: use the shortest member
			sort.Slice(members, func(i, j int) bool { return len(members[i]) < len(members[j]) })
			name = known[members[0]]
		}
		for _, k := range members {
			m[k] = name
		}
	}
	for k, t := range known {
		if m[k] == "" {
			m[k] = t
		}
	}
	return m, nil
}

var profileSchema = map[string]any{"type": "object", "required": []string{"profile"},
	"properties": map[string]any{"profile": map[string]any{"type": "array", "minItems": 1, "maxItems": profileLines,
		"items": map[string]any{"type": "string", "maxLength": 150}}}}

const profileSystem = "You keep a short profile of one household's shopping habits for their list app: one habit " +
	"per line, at most 12 lines, each a short sentence. Write in the language most of the item names are in. " +
	"Use only the facts given: never invent products, habits or numbers, and copy any number exactly from the " +
	"facts. Copy product names exactly as written; never translate them. Start from the previous profile: keep " +
	"what the household wrote in it unless the facts contradict it. Answer with compact one-line JSON."

// One worked example (it never changes, so the server can cache it with the system prompt). Its
// products are unusual on purpose: a line naming one of them was copied, not learned.
var profileExample = []llmMsg{
	{"user", "Today is Monday 2026-03-02.\nPurchases per product:\n- Cat food: 6 purchases, about every 6 days, last 2 days ago; often with Batteries\n" +
		"- Batteries: 2 purchases, about every 45 days, last 20 days ago; often with Cat food\n\nPrevious profile:\nNo pork"},
	{"assistant", `{"profile":["No pork","Cat food about every 6 days, often with batteries","Batteries about every 45 days"]}`},
}

var exampleWords = []string{"cat food", "batteries", "pork"}

var digits = regexp.MustCompile(`\d+`)

// copiedLine reports whether a profile line mentions the worked example's products while the
// real facts (and previous profile) do not.
func copiedLine(l, facts string) bool {
	l, facts = normTitle(l), normTitle(facts)
	for _, w := range exampleWords {
		if strings.Contains(l, w) && !strings.Contains(facts, w) {
			return true
		}
	}
	return false
}

// factLines lists the statistics the profile may use, most bought first.
func factLines(stats map[string]*groupStat, limit int) []string {
	gs := make([]*groupStat, 0, len(stats))
	for _, g := range stats {
		gs = append(gs, g)
	}
	sort.Slice(gs, func(i, j int) bool {
		if gs[i].Count != gs[j].Count {
			return gs[i].Count > gs[j].Count
		}
		return gs[i].Name < gs[j].Name
	})
	var out []string
	for _, g := range gs {
		if len(out) >= limit || g.Count < 2 { // one purchase is not a habit
			break
		}
		since := "today"
		if g.DaysSince > 0 {
			since = plural(g.DaysSince, "day", "days") + " ago"
		}
		l := fmt.Sprintf("- %s: %s, ", g.Name, plural(g.Count, "purchase", "purchases"))
		if g.Median > 0 {
			l += fmt.Sprintf("about every %s, ", plural(int(math.Round(g.Median)), "day", "days"))
		}
		l += "last " + since
		if len(g.Companions) > 0 {
			l += "; often with " + strings.Join(g.Companions, ", ")
		}
		out = append(out, l)
	}
	return out
}

// updateProfile rewrites the profile from the history. Lines quoting a number that is not in the
// facts are dropped, since the model must not make up rhythms.
func (ln *learner) updateProfile(ctx context.Context, now time.Time) error {
	hist := ln.store.History(0, historyMax)
	ln.mu.Lock()
	stats := ln.statsLocked(hist, now)
	prev := ln.st.Profile
	dis := map[string]int{}
	for _, d := range ln.st.Dismissed {
		if now.Sub(time.UnixMilli(d.At)) < 60*24*time.Hour {
			_, n := ln.nameLocked(d.Name)
			dis[n]++
		}
	}
	ln.mu.Unlock()
	if len(stats) == 0 {
		return nil
	}
	facts := fmt.Sprintf("Today is %s.\nPurchases per product:\n%s\n", now.In(london).Format("Monday 2006-01-02"), strings.Join(factLines(stats, 40), "\n"))
	if len(dis) > 0 {
		var ds []string
		for n, c := range dis {
			ds = append(ds, fmt.Sprintf("- %s: dismissed %s", n, plural(c, "time", "times")))
		}
		sort.Strings(ds)
		facts += "Suggestions the household dismissed lately:\n" + strings.Join(ds, "\n") + "\n"
	}
	if prev == "" {
		prev = "(none yet)"
	}
	var out struct {
		Profile []string `json:"profile"`
	}
	msgs := append(append([]llmMsg{{"system", profileSystem}}, profileExample...), llmMsg{"user", facts + "\nPrevious profile:\n" + prev})
	if err := ln.llm.chat(ctx, msgs, profileSchema, &out); err != nil {
		return err
	}
	allowed := map[string]bool{}
	for _, d := range digits.FindAllString(facts+prev, -1) {
		allowed[d] = true
	}
	var kept []string
	for _, l := range out.Profile {
		// Drop lines copied from the worked example, and lines quoting a number not in the facts.
		ok := !copiedLine(l, facts+"\n"+prev)
		for _, d := range digits.FindAllString(l, -1) {
			ok = ok && allowed[d]
		}
		if ok {
			kept = append(kept, l)
		}
	}
	if len(kept) == 0 {
		return fmt.Errorf("%w: unusable profile", errLLMOutput)
	}
	ln.mu.Lock()
	// The person's own lines first, then what was learned, without repeats.
	lines, seen := []string{}, map[string]bool{}
	for _, l := range append(append([]string{}, ln.st.Pinned...), kept...) {
		if l = strings.TrimSpace(l); l != "" && !seen[normTitle(l)] && len(lines) < profileLines {
			seen[normTitle(l)] = true
			lines = append(lines, l)
		}
	}
	ln.st.Profile, ln.st.ProfileAt, ln.st.Edited = strings.Join(lines, "\n"), now.UnixMilli(), false
	ln.st.ProfileDay = now.In(london).Format("2006-01-02")
	ln.saveLocked()
	ln.mu.Unlock()
	return nil
}

const suggestSystem = "You help a household's shopping list app decide what to suggest. From the candidates only, " +
	"pick up to 5 things the household probably needs to buy now, most likely first; use the profile and the " +
	"coming todos (a dinner, guests, a trip). Answer with compact one-line JSON."

// pickSuggestions lets the LLM choose and rank among the code's candidates. (Its own reasons were
// tried and dropped: at 1.7B they were confident nonsense, e.g. toilet paper "for the dinner".)
func (ln *learner) pickSuggestions(ctx context.Context, now time.Time) error {
	hist, items, rev := ln.store.History(0, historyMax), ln.store.List(""), ln.store.Rev()
	ln.mu.Lock()
	cands := ln.candidatesLocked(ln.statsLocked(hist, now), items, now)
	profile := ln.st.Profile
	ln.mu.Unlock()
	if len(cands) == 0 {
		ln.setPicks([]Suggestion{}, rev, now)
		return nil
	}
	names := make([]string, len(cands))
	var cl []string
	for i, c := range cands {
		names[i] = c.Name
		cl = append(cl, "- "+c.Name+": "+c.reason())
	}
	today := now.In(london)
	var open, todos []string
	for _, it := range items {
		switch {
		case it.Done:
		case it.Tab == "shopping":
			open = append(open, it.Title)
		case it.TargetDate != "" && it.TargetDate >= today.Format("2006-01-02") && it.TargetDate <= today.AddDate(0, 0, 7).Format("2006-01-02"):
			d, _ := time.ParseInLocation("2006-01-02", it.TargetDate, london)
			todos = append(todos, "- "+it.Title+" ("+d.Weekday().String()+")")
		}
	}
	if profile == "" {
		profile = "(none yet)"
	}
	user := fmt.Sprintf("Today is %s.\nCandidates:\n%s\nAlready on the shopping list: %s\nTodos this week:\n%s\nProfile:\n%s",
		today.Weekday(), strings.Join(cl, "\n"), orNone(strings.Join(open, ", ")), orNone(strings.Join(todos, "\n")), profile)
	schema := map[string]any{"type": "object", "required": []string{"suggestions"},
		"properties": map[string]any{"suggestions": map[string]any{"type": "array", "maxItems": maxSuggestions, "items": map[string]any{
			"type": "object", "required": []string{"name"},
			"properties": map[string]any{"name": map[string]any{"type": "string", "enum": names}}}}}}
	var out struct {
		Suggestions []Suggestion `json:"suggestions"`
	}
	if err := ln.llm.chat(ctx, []llmMsg{{"system", suggestSystem}, {"user", user}}, schema, &out); err != nil {
		return err
	}
	ok := map[string]bool{}
	for _, n := range names {
		ok[normTitle(n)] = true
	}
	picks, used := []Suggestion{}, map[string]bool{}
	for _, s := range out.Suggestions {
		k := normTitle(s.Name)
		if !ok[k] {
			return fmt.Errorf("%w: %q is not a candidate", errLLMOutput, s.Name)
		}
		if used[k] {
			continue
		}
		used[k] = true
		picks = append(picks, Suggestion{Name: s.Name})
		if len(picks) == maxSuggestions {
			break
		}
	}
	ln.setPicks(picks, rev, now)
	return nil
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func (ln *learner) setPicks(p []Suggestion, rev int64, now time.Time) {
	ln.mu.Lock()
	ln.picks, ln.picksRv, ln.picksAt = p, rev, now
	ln.mu.Unlock()
}

// ---- background job ----

// Refresh asks the job for a run now, the profile included; nil-safe and never blocks.
func (ln *learner) Refresh() {
	select {
	case ln.kick <- true:
	default:
	}
}

func (ln *learner) run() {
	t := time.NewTicker(learnEvery)
	force := false
	for {
		ln.step(time.Now(), force)
		select {
		case <-t.C:
			force = false
		case force = <-ln.kick:
		}
	}
}

// step regroups new names, updates the profile once each morning (07:00–08:00 London, after the
// LLM wakes; the day is skipped if it stays down) or when asked, and refreshes the picks when the
// lists changed or they are old.
func (ln *learner) step(now time.Time, force bool) {
	if !ln.llm.Available() {
		return
	}
	// A step whose answer was rejected waits retryAfter before trying again (unless asked).
	run := func(what string, f func(ctx context.Context) error) bool {
		if !force && now.Sub(ln.tried[what]) < retryAfter {
			return true
		}
		ctx, cancel := context.WithTimeout(context.Background(), learnTimeout)
		defer cancel()
		start := time.Now()
		err := f(ctx)
		if err != nil {
			log.Printf("learn: %s outcome=%s duration_ms=%d: %v", what, llmOutcome(err), time.Since(start).Milliseconds(), err)
		} else {
			log.Printf("learn: %s outcome=ok duration_ms=%d", what, time.Since(start).Milliseconds())
		}
		if err != nil {
			ln.tried[what] = now
		} else {
			delete(ln.tried, what)
		}
		return !errors.Is(err, errLLMDown)
	}
	hist := ln.store.History(0, historyMax)
	seen := map[string]bool{}
	var titles, fresh []string
	ln.mu.Lock()
	for _, p := range hist {
		k := normTitle(p.Title)
		if !seen[k] && len(titles) < groupTitlesMax {
			seen[k] = true
			titles = append(titles, strings.TrimSpace(p.Title))
			if _, ok := ln.st.Groups[k]; !ok {
				fresh = append(fresh, k)
			}
		}
	}
	ln.mu.Unlock()
	if len(fresh) > 0 && len(titles) > 1 {
		if !run("group", func(ctx context.Context) error {
			m, err := ln.regroup(ctx, titles)
			if err == nil {
				ln.mu.Lock()
				ln.st.Groups = m
				ln.saveLocked()
				ln.mu.Unlock()
			}
			return err
		}) {
			return
		}
	}
	t := now.In(london)
	ln.mu.Lock()
	morning := t.Hour() == 7 && ln.st.ProfileDay != t.Format("2006-01-02")
	never := ln.st.ProfileAt == 0
	stale := ln.picks == nil || ln.picksRv != ln.store.Rev() || now.Sub(ln.picksAt) > suggestMaxAge
	ln.mu.Unlock()
	if (force || morning || never) && len(hist) > 0 {
		if !run("profile", func(ctx context.Context) error { return ln.updateProfile(ctx, now) }) {
			return
		}
	}
	if force || stale {
		run("suggest", func(ctx context.Context) error { return ln.pickSuggestions(ctx, now) })
	}
}
