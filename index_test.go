package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRecordsRoundTripAndSkipBadLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pc.jsonl")
	want := []Record{
		{Host: "pc", ID: "a", Cwd: "/home/me/c/x", MTime: time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC),
			Bytes: 10, First: "hi <b>", Last: "bye", Text: "hi <b>\n\nbye"},
		{Host: "pc", ID: "b", Account: recordAccount{Number: 3, Email: "me@x.com"}},
	}
	if err := writeRecordFile(path, want); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("not json\n{\"host\":\"pc\"}\n")
	f.Close()

	got, err := readRecordFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("records = %+v\nwant      %+v", got, want)
	}
}

func TestAMissingRecordFileIsEmpty(t *testing.T) {
	got, err := readRecordFile(filepath.Join(t.TempDir(), "nobody.jsonl"))
	if err != nil || got != nil {
		t.Errorf("missing file = %v, %v", got, err)
	}
}

func TestARecordBecomesASearchableSession(t *testing.T) {
	r := Record{Host: "pc", ID: "a", Cwd: "/home/me/c/x", Text: "Deep In The Middle",
		Account: recordAccount{Number: 2, Email: "me@x.com"}}
	s := r.session()
	if s.Host != "pc" || s.ConvID != "a" || s.Account.number != 2 || s.Account.email != "me@x.com" {
		t.Errorf("session = %+v", s)
	}
	if !strings.Contains(s.haystack, "deep in the middle") || !strings.Contains(s.haystack, "pc") {
		t.Errorf("haystack = %q", s.haystack)
	}
}

func TestLocalHostIsTheShortLowercaseName(t *testing.T) {
	t.Setenv("CLAUDE_SESSIONS_HOST", "PC")
	if got := localHost(); got != "pc" {
		t.Errorf("override = %q", got)
	}
	t.Setenv("CLAUDE_SESSIONS_HOST", "")
	if got := localHost(); got == "" || strings.Contains(got, ".") || got != strings.ToLower(got) {
		t.Errorf("hostname = %q", got)
	}
}

func TestIndexHostDefaultsToTheMacmini(t *testing.T) {
	t.Setenv("CLAUDE_SESSIONS_INDEX_HOST", "")
	if got := indexHost(); got != "macmini" {
		t.Errorf("index host = %q", got)
	}
}
