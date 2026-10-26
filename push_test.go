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

func TestAFailedSendKeepsThePendingMarkAndLeavesNoStamp(t *testing.T) {
	withInterval(t, 0)
	dir := t.TempDir()
	calls := 0
	p := pusher{dir: dir, send: func() error {
		calls++
		return os.ErrInvalid
	}}
	if err := p.run(); err == nil {
		t.Fatal("run hid the send error")
	}
	if _, err := os.Stat(filepath.Join(dir, "push.stamp")); err == nil {
		t.Error("a stamp was written after a failed push")
	}
	if _, err := os.Stat(filepath.Join(dir, "push.pending")); err != nil {
		t.Error("the pending mark was lost by a failed push")
	}
}

func TestAFutureStampDoesNotSleepPastTheInterval(t *testing.T) {
	withInterval(t, 100*time.Millisecond)
	dir := t.TempDir()
	stamp := filepath.Join(dir, "push.stamp")
	touch(stamp)
	future := time.Now().Add(3 * time.Hour)
	os.Chtimes(stamp, future, future)
	start := time.Now()
	p := pusher{dir: dir, send: func() error { return nil }}
	if err := p.run(); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("slept %v for a stamp in the future", elapsed)
	}
}

// fakeSSH puts an ssh script first on PATH and returns the file where it logs
// each call's arguments.
func fakeSSH(t *testing.T, script string) string {
	t.Helper()
	bin, log := t.TempDir(), filepath.Join(t.TempDir(), "calls")
	body := "#!/bin/sh\necho \"$@\" >> " + log + "\ncat > /dev/null\n" + script
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CLAUDE_SESSIONS_REMOTE", "")
	t.Setenv("CLAUDE_SESSIONS_HOST", "pc")
	t.Setenv("CLAUDE_SESSIONS_INDEX_HOST", "macmini")
	return log
}

func TestTheSSHTransportResyncsWhenTheIndexAsksForIt(t *testing.T) {
	root := writeTranscript(t, "-home-me-c-x", "s1",
		`{"type":"user","cwd":"/home/me/c/x","message":{"content":"hi"}}`)
	t.Setenv("CLAUDE_PROJECTS_DIR", root)
	log := fakeSSH(t, "if [ \"$(wc -l < "+"\"$0.n\" 2>/dev/null || echo 0)\" -eq 0 ]; then echo x >> \"$0.n\"; exit 3; fi\necho '{}'\n")
	cache := t.TempDir()
	if err := writeManifest(filepath.Join(cache, "pushed.json"), manifest{}); err != nil {
		t.Fatal(err)
	}
	if err := pushOnce(cache); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(log)
	if n := strings.Count(string(data), "\n"); n != 2 {
		t.Errorf("ssh ran %d times, want 2 (delta, then full)", n)
	}
	if !strings.Contains(string(data), "-- macmini") {
		t.Errorf("host not separated by --: %s", data)
	}
	want := `-- macmini PATH=/opt/homebrew/bin:/usr/local/go/bin:/usr/local/bin:"$PATH" ~/c/claude-sessions/claude-sessions --ingest 'pc'`
	if !strings.Contains(string(data), want) {
		t.Errorf("remote command lacks the PATH prefix or quoting:\n got %s\nwant %s", data, want)
	}
}

func TestAHungSSHIsCutOff(t *testing.T) {
	old := sshTimeout
	sshTimeout = 300 * time.Millisecond
	t.Cleanup(func() { sshTimeout = old })
	fakeSSH(t, "exec sleep 30\n")
	start := time.Now()
	_, err := ingestAt("pc", strings.NewReader(""))
	if err == nil {
		t.Fatal("a hung ssh reported success")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("took %v to give up", elapsed)
	}
}
