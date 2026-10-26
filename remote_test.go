package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRemoteRowsMergeByAgeAndKeepTheCursor(t *testing.T) {
	now := time.Now()
	m := newModel([]Session{
		{ConvID: "l1", Cwd: "/a", MTime: now.Add(-1 * time.Hour)},
		{ConvID: "l2", Cwd: "/b", MTime: now.Add(-3 * time.Hour)},
	}, scope{})
	m.cursor = 1 // on l2

	next, _ := m.Update(remoteMsg{live: true, sessions: []Session{
		{ConvID: "r1", Host: "pc", Cwd: "/home/me/c", MTime: now.Add(-2 * time.Hour)},
		{ConvID: "r0", Host: "pc", Cwd: "/home/me/c", MTime: now},
	}})
	got := next.(model)

	var order []string
	for _, s := range got.filtered {
		order = append(order, s.ConvID)
	}
	if strings.Join(order, " ") != "r0 l1 r1 l2" {
		t.Errorf("order = %v", order)
	}
	if sel := got.selected(); sel == nil || sel.ConvID != "l2" {
		t.Errorf("cursor moved to %+v", sel)
	}
}

func TestALateCacheReadDoesNotReplaceLiveRows(t *testing.T) {
	m := newModel(nil, scope{})
	next, _ := m.Update(remoteMsg{live: true, sessions: []Session{{ConvID: "fresh", Host: "pc"}}})
	next, _ = next.(model).Update(remoteMsg{synced: time.Now().Add(-time.Hour), sessions: []Session{{ConvID: "stale", Host: "pc"}}})
	got := next.(model)
	if len(got.sessions) != 1 || got.sessions[0].ConvID != "fresh" {
		t.Errorf("sessions = %+v", got.sessions)
	}
	if !strings.Contains(got.titleLine(), "remote: live") {
		t.Errorf("title = %q", got.titleLine())
	}
}

func TestTheTitleSaysHowOldTheRemoteRowsAre(t *testing.T) {
	m := newModel(nil, scope{})
	if strings.Contains(m.titleLine(), "remote:") {
		t.Errorf("no remote data, yet the title says %q", m.titleLine())
	}
	next, _ := m.Update(remoteMsg{synced: m.now.Add(-3 * time.Hour), sessions: []Session{{ConvID: "r", Host: "pc"}}})
	if title := next.(model).titleLine(); !strings.Contains(title, "remote: cached 3h ago") {
		t.Errorf("title = %q", title)
	}
}

func TestRemoteRowsShowTheirHostAndMatchIt(t *testing.T) {
	m := newModel([]Session{{ConvID: "l", Cwd: "/a", First: "x", Last: "y"}}, scope{})
	m.width = 120
	next, _ := m.Update(remoteMsg{live: true, sessions: []Session{{ConvID: "r", Host: "pc", Cwd: "/home/me/c", First: "x", Last: "y"}}})
	got := next.(model)

	var row string
	for _, s := range got.filtered {
		if s.ConvID == "r" {
			row = got.renderRow(s, false)
		}
	}
	if !strings.Contains(row, "pc") {
		t.Errorf("remote row has no host: %q", row)
	}
	got.query = "pc"
	got.applyFilter()
	if len(got.filtered) != 1 || got.filtered[0].ConvID != "r" {
		t.Errorf("filtering by host kept %d rows", len(got.filtered))
	}
}

func TestRemoteRowsRespectTheDirectoryScope(t *testing.T) {
	m := newModel(nil, scope{path: "/Users/me/c/proj"})
	next, _ := m.Update(remoteMsg{live: true, sessions: []Session{
		{ConvID: "in", Host: "pc", Cwd: "/home/me/c/proj/sub"},
		{ConvID: "out", Host: "pc", Cwd: "/home/me/c/other"},
	}})
	got := next.(model)
	if len(got.sessions) != 1 || got.sessions[0].ConvID != "in" {
		t.Errorf("sessions = %+v", got.sessions)
	}
}

func TestTheRemoteSourceDropsThisHostAndBadLines(t *testing.T) {
	dir := t.TempDir()
	writeRecordFile(filepath.Join(dir, "pc.jsonl"), []Record{{Host: "pc", ID: "a"}})
	writeRecordFile(filepath.Join(dir, "immunemac.jsonl"), []Record{{Host: "immunemac", ID: "b"}})
	f, _ := os.OpenFile(filepath.Join(dir, "pc.jsonl"), os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("garbage\n")
	f.Close()

	sessions := remoteSource{dir: dir, self: "immunemac"}.read()
	if len(sessions) != 1 || sessions[0].ConvID != "a" || sessions[0].Host != "pc" {
		t.Errorf("sessions = %+v", sessions)
	}
}

func TestFetchSyncsTheMirrorWithRsync(t *testing.T) {
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skip("rsync not installed")
	}
	t.Setenv("HOME", t.TempDir())
	index := t.TempDir()
	t.Setenv("CLAUDE_SESSIONS_REMOTE", index)
	t.Setenv("CLAUDE_SESSIONS_HOST", "immunemac")
	writeRecordFile(filepath.Join(index, "pc.jsonl"), []Record{{Host: "pc", ID: "a", MTime: time.Now()}})
	writeManifest(filepath.Join(index, "pc.manifest.json"), manifest{})

	src, err := newRemoteSource()
	if err != nil {
		t.Fatal(err)
	}
	live, ok := fetchRemote(src)().(remoteMsg)
	if !ok || !live.live || len(live.sessions) != 1 {
		t.Fatalf("fetch = %+v", live)
	}
	if _, err := os.Stat(filepath.Join(src.dir, "pc.manifest.json")); !os.IsNotExist(err) {
		t.Error("the manifest was mirrored")
	}
	cached := loadCachedRemote(src)().(remoteMsg)
	if cached.live || len(cached.sessions) != 1 || time.Since(cached.synced) > time.Minute {
		t.Errorf("cached = %+v", cached)
	}
}

func TestHomeRelativeMatchesMacAndLinuxHomes(t *testing.T) {
	for in, want := range map[string]string{
		"/Users/me/c/x": "~/c/x",
		"/home/me/c/x":  "~/c/x",
		"/home/me":      "~",
		"/opt/x":        "/opt/x",
	} {
		if got := homeRelative(in); got != want {
			t.Errorf("homeRelative(%q) = %q, want %q", in, got, want)
		}
	}
}
