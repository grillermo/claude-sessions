// The other machines' sessions, read from a mirror of the index host's files.
//
// Startup reads the mirror as it is, so those rows show up at once, and an
// rsync in the background brings it up to date: only the changed parts of each
// host's file cross the network, compressed. On the index host itself there is
// no mirror; it reads the index directly.
package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// remoteMsg carries the other machines' sessions.
type remoteMsg struct {
	sessions []Session
	synced   time.Time // when the mirror last matched the index host
	live     bool      // read right after a successful sync
}

// remoteSource is where the other machines' host files are read from.
type remoteSource struct {
	dir      string // directory of <host>.jsonl files
	rsyncSrc string // what to rsync dir from; "" when dir is the index itself
	self     string // this machine, whose rows come from its own transcripts
}

// fetchTimeout bounds the background rsync.
var fetchTimeout = 30 * time.Second

func newRemoteSource() (remoteSource, error) {
	cache, err := cacheDir()
	if err != nil {
		return remoteSource{}, err
	}
	self := localHost()
	if dir := os.Getenv("CLAUDE_SESSIONS_REMOTE"); dir != "" {
		return remoteSource{dir: filepath.Join(cache, "remote"), rsyncSrc: strings.TrimRight(dir, "/") + "/", self: self}, nil
	}
	if self == indexHost() {
		return remoteSource{dir: filepath.Join(cache, "hosts"), self: self}, nil
	}
	return remoteSource{
		dir:      filepath.Join(cache, "remote"),
		rsyncSrc: indexHost() + ":.cache/claude-sessions/hosts/",
		self:     self,
	}, nil
}

// read loads every host file in the source, leaving out this machine's rows:
// its own transcripts are newer than anything it pushed.
func (r remoteSource) read() []Session {
	paths, _ := filepath.Glob(filepath.Join(r.dir, "*.jsonl"))
	var sessions []Session
	for _, path := range paths {
		records, err := readRecordFile(path)
		if err != nil {
			continue
		}
		for _, rec := range records {
			if rec.Host == "" || rec.Host == r.self {
				continue
			}
			sessions = append(sessions, rec.session())
		}
	}
	return sessions
}

// syncedAt is when the mirror last matched the index host.
func (r remoteSource) syncedAt() time.Time {
	if r.rsyncSrc == "" {
		return time.Now()
	}
	info, err := os.Stat(filepath.Join(r.dir, ".synced"))
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
}

// sync brings the mirror up to date. rsync writes each file under a temporary
// name and renames it, so a sync that dies halfway leaves whole files behind.
func (r remoteSource) sync() error {
	if err := os.MkdirAll(r.dir, 0o755); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "rsync", "-az", "--delete",
		"--include=*.jsonl", "--exclude=*",
		"-e", "ssh -o BatchMode=yes -o ConnectTimeout=5",
		r.rsyncSrc, r.dir+"/")
	if err := cmd.Run(); err != nil {
		return err
	}
	return touch(filepath.Join(r.dir, ".synced"))
}

// loadCachedRemote reads the mirror as it is.
func loadCachedRemote(r remoteSource) tea.Cmd {
	return func() tea.Msg {
		return remoteMsg{sessions: r.read(), synced: r.syncedAt(), live: r.rsyncSrc == ""}
	}
}

// fetchRemote syncs the mirror and reads it. A failed sync sends nothing, so
// the cached rows stay and the title keeps saying how old they are.
func fetchRemote(r remoteSource) tea.Cmd {
	if r.rsyncSrc == "" {
		return nil
	}
	return func() tea.Msg {
		if err := r.sync(); err != nil {
			return nil
		}
		return remoteMsg{sessions: r.read(), synced: r.syncedAt(), live: true}
	}
}

var homePrefix = regexp.MustCompile(`^/(Users|home)/[^/]+`)

// homeRelative writes a path under a home directory as ~/…, so a directory on
// the Mac and the same directory on the PC compare equal: the repos under ~/c
// mirror each other, but the homes are /Users/… and /home/….
func homeRelative(path string) string {
	return homePrefix.ReplaceAllString(path, "~")
}

// coversRemote is coversPath for a session from another machine.
func (s scope) coversRemote(cwd string) bool {
	if s.path == "" {
		return true
	}
	return scope{path: homeRelative(s.path)}.coversPath(homeRelative(cwd))
}
