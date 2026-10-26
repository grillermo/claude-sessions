// The subcommands that keep the shared index: --push on every machine (run by
// the hook and the timer), --ingest on the index host, --export for a look at
// what a push would send.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// runCommand runs a subcommand and reports false when args name none.
func runCommand(args []string) (exit int, handled bool) {
	if len(args) == 0 {
		return 0, false
	}
	switch args[0] {
	case "--export":
		return runExport(args[1:]), true
	case "--ingest":
		return runIngest(args[1:]), true
	case "--push":
		runPush()
		return 0, true
	}
	return 0, false
}

// runExport writes the delta a push would send now; --full writes everything.
func runExport(args []string) int {
	full := len(args) == 1 && args[0] == "--full"
	cache, err := cacheDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "claude-sessions:", err)
		return 1
	}
	sources, err := projectSources()
	if err != nil {
		fmt.Fprintln(os.Stderr, "claude-sessions:", err)
		return 1
	}
	records, err := exportRecords(sources, localHost(), filepath.Join(cache, "local.json"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "claude-sessions:", err)
		return 1
	}
	pushed, _, _ := readManifest(filepath.Join(cache, "pushed.json"))
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	if err := writeDelta(out, records, pushed, full); err != nil {
		fmt.Fprintln(os.Stderr, "claude-sessions:", err)
		return 1
	}
	return 0
}

// runIngest applies a delta from stdin for the named host and prints that
// host's new manifest. It exits resyncExit when the host must push in full.
func runIngest(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: claude-sessions --ingest <host>")
		return 1
	}
	dir := os.Getenv("CLAUDE_SESSIONS_REMOTE")
	if dir == "" {
		cache, err := cacheDir()
		if err != nil {
			fmt.Fprintln(os.Stderr, "claude-sessions:", err)
			return 1
		}
		dir = filepath.Join(cache, "hosts")
	}
	got, err := store{dir}.ingest(args[0], os.Stdin, capBytes())
	if errors.Is(err, errResync) {
		return resyncExit
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "claude-sessions:", err)
		return 1
	}
	if err := json.NewEncoder(os.Stdout).Encode(got); err != nil {
		return 1
	}
	return 0
}

// runPush never fails loudly: it runs behind Claude Code's hooks and a timer,
// so a failure is logged and the next run tries again.
func runPush() {
	cache, err := cacheDir()
	if err != nil {
		return
	}
	if err := os.MkdirAll(cache, 0o755); err != nil {
		return
	}
	p := pusher{dir: cache, send: func() error { return pushOnce(cache) }}
	if err := p.run(); err != nil {
		appendLog(filepath.Join(cache, "push.log"), time.Now().Format(time.RFC3339)+" "+err.Error())
	}
}
