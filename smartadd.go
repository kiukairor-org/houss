// Smart add, the part that needs no LLM: checking the model's answer against the note and the
// app's real lists, and turning day words into dates. The LLM client is in llm.go.
package main

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	_ "time/tzdata" // the distroless image has no zoneinfo; "today" is a London date
	"unicode"
)

const maxQuickItems = 10

var (
	errLLMOutput = errors.New("llm output rejected")
	london       = mustLocation("Europe/London")
)

func mustLocation(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}

// The model names a day ("friday"); the date arithmetic is done here, where it is reliable.
var whenWords = []string{"", "today", "tomorrow", "monday", "tuesday", "wednesday", "thursday", "friday",
	"saturday", "sunday", "next_week", "next_month"}

type quickItem struct {
	Title string `json:"title"`
	List  string `json:"list"`
	When  string `json:"when"`
}

// checkQuick accepts the model's answer only if it fits the app's real lists and loses nothing:
// items made of words that are not in the note are dropped (small models echo the examples), and
// every meaningful word of the note must end up in some title. Otherwise the whole answer is
// rejected and the note is added as typed.
func checkQuick(note string, got []quickItem, now time.Time) ([]Item, error) {
	if len(got) == 0 || len(got) > maxQuickItems {
		return nil, fmt.Errorf("%w: %d items", errLLMOutput, len(got))
	}
	noteWords := map[string]bool{}
	dated := false
	for _, w := range words(note) {
		noteWords[w] = true
		dated = dated || dayWords[w]
	}
	covered := map[string]bool{}
	var out []Item
	for _, q := range got {
		title := strings.TrimSpace(q.Title)
		tw := words(title)
		if title == "" || len(title) > 200 || !tabs[q.List] || len(tw) == 0 {
			return nil, fmt.Errorf("%w: bad item", errLLMOutput)
		}
		grounded, onlyDays := true, true
		for _, w := range tw {
			grounded = grounded && noteWords[w]
			onlyDays = onlyDays && (dayWords[w] || fillerWords[w])
		}
		if !grounded || onlyDays { // invented, or just the day split off ("lundi prochain")
			continue
		}
		due, ok := resolveWhen(q.When, now)
		if !ok {
			return nil, fmt.Errorf("%w: unknown when", errLLMOutput)
		}
		if !dated { // the model sometimes adds "today" to notes that name no day
			due = ""
		}
		for _, w := range tw {
			covered[w] = true
		}
		out = append(out, Item{Title: title, Tab: q.List, TargetDate: due})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: no items left", errLLMOutput)
	}
	for w := range noteWords {
		if !covered[w] && !dayWords[w] && !fillerWords[w] && !linkWords[w] && !clockTime.MatchString(w) {
			return nil, fmt.Errorf("%w: a word of the note is in no item", errLLMOutput)
		}
	}
	return out, nil
}

// words lower-cases s and splits it on anything that is not a letter or a digit.
func words(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

var dayWords = set("today", "tonight", "tomorrow", "monday", "tuesday", "wednesday", "thursday", "friday",
	"saturday", "sunday", "week", "weekend", "month", "morning", "afternoon", "evening",
	"aujourd", "hui", "soir", "demain", "lundi", "mardi", "mercredi", "jeudi", "vendredi", "samedi",
	"dimanche", "semaine", "mois", "matin", "midi")

// linkWords may be left out of titles ("milk and eggs" -> "milk", "eggs"; "acheter des piles" -> "piles").
var linkWords = set("and", "also", "then", "the", "a", "an", "some", "of", "to", "at", "in", "with", "buy", "get", "need",
	"we", "ok", "so", "please", "et", "aussi", "puis", "de", "des", "du", "les", "un", "une", "au", "aux", "à", "avec",
	"dans", "acheter", "prendre", "faut", "il", "on", "stp", "svp")

// clockTime matches times the app has no field for ("3pm", "14h", "14h30"); the day is kept.
var clockTime = regexp.MustCompile(`^\d{1,2}(am|pm|h|h\d\d)$`)

var fillerWords = set("next", "this", "on", "by", "before", "for", "prochain", "prochaine", "ce", "cette", "le", "la", "pour", "avant")

func set(ws ...string) map[string]bool {
	m := make(map[string]bool, len(ws))
	for _, w := range ws {
		m[w] = true
	}
	return m
}

var weekdays = map[string]time.Weekday{"sunday": time.Sunday, "monday": time.Monday, "tuesday": time.Tuesday,
	"wednesday": time.Wednesday, "thursday": time.Thursday, "friday": time.Friday, "saturday": time.Saturday}

// resolveWhen turns the model's day word into a YYYY-MM-DD date in London. A weekday means its
// next occurrence after today ("lundi prochain" said on a Monday is next week's Monday).
func resolveWhen(when string, now time.Time) (string, bool) {
	t := now.In(london)
	day := time.Date(t.Year(), t.Month(), t.Day(), 12, 0, 0, 0, london)
	switch when {
	case "":
		return "", true
	case "today":
	case "tomorrow":
		day = day.AddDate(0, 0, 1)
	case "next_week":
		day = day.AddDate(0, 0, 7-(int(day.Weekday())+6)%7)
	case "next_month":
		day = time.Date(t.Year(), t.Month()+1, 1, 12, 0, 0, 0, london)
	default:
		wd, ok := weekdays[when]
		if !ok {
			return "", false
		}
		ahead := (int(wd) - int(day.Weekday()) + 7) % 7
		if ahead == 0 {
			ahead = 7
		}
		day = day.AddDate(0, 0, ahead)
	}
	return day.Format("2006-01-02"), true
}
