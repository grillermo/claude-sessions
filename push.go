// Pushing this machine's index to the index host: debounced, so a burst of
// turns costs one push, and incremental, so a push carries only what changed.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// pushInterval is the least time between two pushes from one machine.
var pushInterval = time.Minute

// pusher runs send under push.lock in dir, at most once per pushInterval,
// again for as long as new pushes are asked for while one is running.
type pusher struct {
	dir  string
	send func() error
}

func (p pusher) run() error {
	pending := filepath.Join(p.dir, "push.pending")
	if err := touch(pending); err != nil {
		return err
	}
	unlock, ok, err := lockFile(filepath.Join(p.dir, "push.lock"), false)
	if err != nil {
		return err
	}
	if !ok {
		// Another push is running; it sees the pending mark and goes again.
		return nil
	}
	defer unlock()

	stamp := filepath.Join(p.dir, "push.stamp")
	for {
		if info, err := os.Stat(stamp); err == nil {
			if wait := pushInterval - time.Since(info.ModTime()); wait > 0 {
				time.Sleep(wait)
			}
		}
		os.Remove(pending)
		if err := p.send(); err != nil {
			return err
		}
		if err := touch(stamp); err != nil {
			return err
		}
		if _, err := os.Stat(pending); err != nil {
			return nil
		}
	}
}

// touch creates path if needed and sets its mtime to now.
func touch(path string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	file.Close()
	now := time.Now()
	return os.Chtimes(path, now, now)
}

// pushOnce exports this machine's sessions and sends the index host what
// changed since the last push. When the index host does not recognise the
// manifest the delta was built against, it sends everything instead.
func pushOnce(cache string) error {
	sources, err := projectSources()
	if err != nil {
		return err
	}
	host := localHost()
	records, err := exportRecords(sources, host, filepath.Join(cache, "local.json"))
	if err != nil {
		return err
	}
	pushedPath := filepath.Join(cache, "pushed.json")
	pushed, havePushed, err := readManifest(pushedPath)
	if err != nil {
		havePushed = false
	}
	full := !havePushed
	for {
		var delta bytes.Buffer
		if err := writeDelta(&delta, records, pushed, full); err != nil {
			return err
		}
		got, err := ingestAt(host, &delta)
		if errors.Is(err, errResync) && !full {
			full = true
			continue
		}
		if err != nil {
			return err
		}
		return writeManifest(pushedPath, got)
	}
}

// ingestAt hands a delta to the index: in this process when this machine is
// the index host or CLAUDE_SESSIONS_REMOTE names a local index directory,
// otherwise over ssh to the index host's own claude-sessions.
func ingestAt(host string, delta io.Reader) (manifest, error) {
	if dir := os.Getenv("CLAUDE_SESSIONS_REMOTE"); dir != "" {
		return store{dir}.ingest(host, delta, capBytes())
	}
	if host == indexHost() {
		cache, err := cacheDir()
		if err != nil {
			return nil, err
		}
		return store{filepath.Join(cache, "hosts")}.ingest(host, delta, capBytes())
	}

	cmd := exec.Command("ssh", "-C", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", indexHost(),
		"~/c/claude-sessions/claude-sessions --ingest "+shellQuote(host))
	cmd.Stdin = delta
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == resyncExit {
		return nil, errResync
	}
	if err != nil {
		return nil, fmt.Errorf("ssh %s: %v: %s", indexHost(), err, strings.TrimSpace(stderr.String()))
	}
	got := manifest{}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		return nil, fmt.Errorf("ssh %s: unreadable manifest: %w", indexHost(), err)
	}
	return got, nil
}

// appendLog adds a line to the push log and keeps only its last 100 lines.
func appendLog(path, line string) {
	data, _ := os.ReadFile(path)
	var lines []string
	if text := strings.TrimRight(string(data), "\n"); text != "" {
		lines = strings.Split(text, "\n")
	}
	lines = append(lines, line)
	if len(lines) > 100 {
		lines = lines[len(lines)-100:]
	}
	os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
}
