package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func withInterval(t *testing.T, d time.Duration) {
	t.Helper()
	old := pushInterval
	pushInterval = d
	t.Cleanup(func() { pushInterval = old })
}

func TestAPendingMarkDuringAPushCausesOneMoreRound(t *testing.T) {
	withInterval(t, 0)
	dir := t.TempDir()
	calls := 0
	p := pusher{dir: dir, send: func() error {
		calls++
		if calls == 1 {
			touch(filepath.Join(dir, "push.pending"))
		}
		return nil
	}}
	if err := p.run(); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("sent %d times, want 2", calls)
	}
	if _, err := os.Stat(filepath.Join(dir, "push.stamp")); err != nil {
		t.Error("no stamp after a successful push")
	}
}

func TestASecondPushLeavesTheWorkToTheOneHoldingTheLock(t *testing.T) {
	dir := t.TempDir()
	unlock, ok, err := lockFile(filepath.Join(dir, "push.lock"), false)
	if err != nil || !ok {
		t.Fatalf("lock: %v %v", ok, err)
	}
	defer unlock()

	calls := 0
	p := pusher{dir: dir, send: func() error { calls++; return nil }}
	if err := p.run(); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Errorf("sent %d times while another push held the lock", calls)
	}
	if _, err := os.Stat(filepath.Join(dir, "push.pending")); err != nil {
		t.Error("the pending mark was not left for the holder")
	}
}

func TestAPushWaitsOutTheInterval(t *testing.T) {
	withInterval(t, 300*time.Millisecond)
	dir := t.TempDir()
	touch(filepath.Join(dir, "push.stamp"))
	start := time.Now()
	p := pusher{dir: dir, send: func() error { return nil }}
	if err := p.run(); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 250*time.Millisecond {
		t.Errorf("pushed after %v, before the interval was up", elapsed)
	}
}

func TestPushOnceFallsBackToAFullPushWhenTheIndexForgot(t *testing.T) {
	root := writeTranscript(t, "-home-me-c-x", "s1",
		`{"type":"user","cwd":"/home/me/c/x","message":{"content":"hello from the pc"}}`)
	cache, index := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_PROJECTS_DIR", root)
	t.Setenv("CLAUDE_SESSIONS_REMOTE", index)
	t.Setenv("CLAUDE_SESSIONS_HOST", "pc")

	if err := pushOnce(cache); err != nil {
		t.Fatal(err)
	}
	records, _ := readRecordFile(filepath.Join(index, "pc.jsonl"))
	if len(records) != 1 || records[0].First != "hello from the pc" {
		t.Fatalf("first push stored %+v", records)
	}

	// The index loses this host: the next delta no longer matches anything.
	os.Remove(filepath.Join(index, "pc.manifest.json"))
	os.Remove(filepath.Join(index, "pc.jsonl"))
	if err := pushOnce(cache); err != nil {
		t.Fatal(err)
	}
	if records, _ = readRecordFile(filepath.Join(index, "pc.jsonl")); len(records) != 1 {
		t.Errorf("after a resync the index holds %d records", len(records))
	}
}

func TestThePushLogKeepsTheLastHundredLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "push.log")
	for i := 0; i < 150; i++ {
		appendLog(path, "line")
	}
	data, _ := os.ReadFile(path)
	if n := strings.Count(string(data), "\n"); n != 100 {
		t.Errorf("log has %d lines", n)
	}
}
