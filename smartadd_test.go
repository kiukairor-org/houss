package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// Monday 2 November 2026, 18:30 in London.
var monday = time.Date(2026, 11, 2, 18, 30, 0, 0, london)

func TestCheckQuick(t *testing.T) {
	// A day split off as its own item is dropped; a "today" on an undated note is ignored.
	got, err := checkQuick("rappeler la banque lundi prochain", []quickItem{
		{"rappeler la banque", "todos", "monday"}, {"lundi prochain", "shopping", ""}}, monday)
	if err != nil || len(got) != 1 || got[0].TargetDate != "2026-11-09" {
		t.Fatalf("got %+v, %v", got, err)
	}
	got, err = checkQuick("acheter des piles", []quickItem{{"piles", "shopping", "today"}}, monday)
	if err != nil || len(got) != 1 || got[0].TargetDate != "" || got[0].Title != "piles" {
		t.Fatalf("got %+v, %v", got, err)
	}
	// An echoed example is dropped; the rest of the answer stands.
	got, err = checkQuick("milk and eggs", []quickItem{{"milk", "shopping", ""}, {"eggs", "shopping", ""}, {"pain", "shopping", ""}}, monday)
	if err != nil || len(got) != 2 {
		t.Fatalf("echo: got %+v, %v", got, err)
	}
	// A clock time has no field; the day still sets the date.
	got, err = checkQuick("dentist thursday 3pm", []quickItem{{"dentist", "todos", "thursday"}}, monday)
	if err != nil || len(got) != 1 || got[0].TargetDate != "2026-11-05" {
		t.Fatalf("time: got %+v, %v", got, err)
	}
	// Losing part of the note rejects the whole answer.
	if _, err := checkQuick("milk, call mum", []quickItem{{"milk", "shopping", ""}}, monday); !errors.Is(err, errLLMOutput) {
		t.Fatal("dropped 'call mum' must be rejected")
	}
	if _, err := checkQuick("milk", nil, monday); !errors.Is(err, errLLMOutput) {
		t.Fatal("no items must be rejected")
	}
	if _, err := checkQuick("milk", []quickItem{{"milk", "shopping", "someday"}}, monday); !errors.Is(err, errLLMOutput) {
		t.Fatal("unknown when must be rejected")
	}
	if _, err := checkQuick("milk", []quickItem{{strings.Repeat("milk ", 50), "shopping", ""}}, monday); !errors.Is(err, errLLMOutput) {
		t.Fatal("over-long title must be rejected")
	}
}

func TestResolveWhen(t *testing.T) {
	for when, want := range map[string]string{
		"": "", "today": "2026-11-02", "tomorrow": "2026-11-03", "friday": "2026-11-06",
		"monday": "2026-11-09", "sunday": "2026-11-08", "next_week": "2026-11-09", "next_month": "2026-12-01",
	} {
		if got, ok := resolveWhen(when, monday); !ok || got != want {
			t.Errorf("%q: got %q want %q", when, got, want)
		}
	}
	// 23:30 UTC on 31 Mar 2026 is already 1 Apr in London (BST).
	if got, _ := resolveWhen("today", time.Date(2026, 3, 31, 23, 30, 0, 0, time.UTC)); got != "2026-04-01" {
		t.Errorf("London date: got %q", got)
	}
	if got, _ := resolveWhen("next_month", time.Date(2026, 12, 15, 9, 0, 0, 0, london)); got != "2027-01-01" {
		t.Errorf("year wrap: got %q", got)
	}
	if _, ok := resolveWhen("someday", monday); ok {
		t.Error("unknown word accepted")
	}
}
