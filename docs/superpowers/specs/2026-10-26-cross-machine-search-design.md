# Cross-machine search for claude-sessions

## Goal

List, search and resume Claude Code sessions from immunemac, macmini and PC-linux in one
claude-sessions timeline. The search covers every prompt and every Claude answer, not only the
first and last message. Startup must stay as fast as it is today; rows from the other machines may
arrive a moment after launch.

## Decisions

- **Hosts in scope: immunemac, macmini, PC-linux.** macaliada, mactastic and PC-windows stay out
  for now: macaliada has no Go on its non-login PATH, and mactastic has no sessions.
- **Architecture: each host pushes its index to the macmini (option C).** The macmini is always on
  and the only host reachable over WARP, so one fetch from it covers every host, and a sleeping
  host stays searchable at its last push. Rejected: SSH to every host on each launch (with or
  without a cache), and syncing raw transcripts (about 850 MB, collides with live writes, and
  pollutes Claude's own `/resume`).
- **Startup: cached remote rows show at once, refreshed in the background.** Startup must stay
  fast, and async updates are acceptable.
- **Search depth: every prompt and every Claude answer (all prose).** This replaces the earlier
  "first + last + prompts capped at 4 KB" choice. Measured on 2026-10-26: about 9 MB of prose across
  the three hosts (immunemac 5.4 MB, macmini 3.1 MB, pc 0.6 MB) out of 846 MB of raw transcripts,
  so it all fits in memory and plain substring search is enough.
- **Cap: the newest 5 GB of history; oldest sessions dropped first.** Raised from 1000 MB; user:
  "increase it to 5GB". User: "when the cap is reached, enough old conversations should be dropped
  to make space for new ones." It's a rolling window, measured on raw transcript bytes across all
  hosts (846 MB today, so nothing is dropped yet). At about 1% prose, a full window holds roughly
  50 MB of searchable text, which still fits in memory. Dropping only removes sessions from the shared index on the macmini; transcripts on disk
  are never deleted. The user approved these defaults with the design.
- **Subagent transcripts are not indexed.** Their output already returns to the parent session.
- **Resume of a row from another machine: `ssh -t` to that host.** That host has the working tree,
  the transcript and the cswap account.
- **Push trigger: a Stop/SessionEnd hook, debounced, plus a 10-minute timer.** The hook keeps the
  index fresh, and the timer catches sessions that end abnormally.
- **The push hook and timer ship through `~/c/agents-configs`, not an installer in this repo.**
  User: "take advantage of the fact that I share my claude settings across all the machines with
  ../agents-configs". Checked: the macmini and pc both have `~/.claude/hooks/` symlinked from
  agents-configs, jq, and the env guard registered. On immunemac each cswap account's
  `settings.json` is a symlink to `~/.claude/settings.json`.

## What exists today

- `listTranscripts` (sessions.go) only `stat`s `<source>/*/*.jsonl` across `~/.claude/projects`
  and each cswap account, then sorts by mtime. This is what keeps startup fast.
- `loadNext` (main.go) parses the pending transcripts in batches of 25 off the UI thread and
  returns `batchMsg`. Every local transcript gets parsed eventually.
- `applyFilter` matches `First`, `Last`, `Cwd` and the account label with a lowercase substring
  test.
- `resumeCommand` builds `cd <cwd> && claude --resume <id>`, or `cswap run N -- --resume <id>` for
  a cswap account, and `main` execs it through `$SHELL -c`.

## Design

### Host names

A host is `hostname -s`, lowercased: `immunemac`, `macmini`, `pc`. These equal the SSH aliases,
so a host name is also what to SSH to. `CLAUDE_SESSIONS_HOST` overrides it. The index host is
`macmini`, which `CLAUDE_SESSIONS_INDEX_HOST` overrides.

### Index record

A host's index is JSON lines, one record per session:

```json
{"host":"pc","id":"…","cwd":"/home/grillermo/c/proj","mtime":"2026-10-26T11:48:52-06:00",
 "bytes":183422,"account":{"number":3,"email":"hola@grillermo.com"},
 "first":"…","last":"…","text":"…"}
```

- `bytes` is the transcript's raw size, the unit the cap is measured in.
- `text` is all prose blocks of the session (user and assistant), in order and joined by blank
  lines. They're extracted with the same rules as `messageTexts`: no tool calls, tool results,
  thinking, meta records or system reminders.
- Unknown fields are ignored when reading, so an older binary can read a newer file.

### Changes to existing code (claude-sessions)

- `readSession` also returns the full text. `Session` gains `Host`, `Text` (as typed) and a
  lowercased `haystack` built once on load from first, last, cwd, account, host and text.
  `applyFilter` matches against `haystack`. Local rows get full-text search too.
- `Init` becomes `tea.Batch(loadNext, loadCachedRemote, fetchRemote)`.
- Rows for this host's name are dropped from remote data, because the live local read is newer.
  Remaining remote rows are merged into the one timeline by mtime. Sessions are keyed by
  `(host, id)`. When rows arrive, the cursor stays on the session ID it was on.
- Remote rows show their host next to the account label in the row header. The filter matches the
  host, so `/pc` narrows the list to the PC.
- The title line gets a dim remote status: `remote: live`, `remote: cached 3h ago`, or nothing when
  there is no remote data at all.
- `resumeCommand` for a remote row returns an `ssh -t <host>` invocation that runs
  `exec "$SHELL" -lic '<cd + claude/cswap command>'` on that host, using the cwd and account
  recorded there. `main` execs `ssh` directly, not through the local shell wrapper. The
  `CLAUDE_SESSIONS_CWD_FILE` cd-back is skipped for remote rows.

### New subcommands (claude-sessions)

- `--export` writes this host's index records to stdout. A parse cache at
  `~/.cache/claude-sessions/local.json`, keyed by transcript path and storing `(mtime, size,
  record)`, means only new or changed transcripts get parsed again. The first run parses
  everything (about 2s on immunemac); later runs cost about as much as today's `stat` listing.
- `--ingest <host>` runs on the macmini. It reads records from stdin and replaces that host's set
  of sessions. It then applies the cap across all hosts and rewrites the changed files in
  `~/.cache/claude-sessions/hosts/<host>.jsonl` (temp file plus rename). An `flock` on that
  directory serializes concurrent ingests.
- `--push` is `--export | ssh -C macmini '~/c/claude-sessions/claude-sessions --ingest <host>'`.
  On the macmini it pipes to `--ingest` in-process, with no SSH. It's described under Push.

### Cap

Every ingest rebuilds the full set of sessions across all hosts, sorts it newest first by mtime
(ties broken by `(host, id)`), adds up `bytes`, and keeps sessions until the next one would push
the total over 5 GB (5 × 1024 × 1024 × 1024 bytes). It drops every older session. Nothing more is
dropped than needed. A dropped session that a host pushes again is still the oldest, so the same
ingest drops it again; no cutoff state has to be stored. The cap is a constant, overridable with
`CLAUDE_SESSIONS_CAP_MB` for tests.

### Push

1. When a turn ends, the Stop or SessionEnd hook runs
   `~/.claude/hooks/claude-sessions-push.sh`. Every 10 minutes the timer runs the same script.
2. The script runs `~/c/claude-sessions/claude-sessions --push` with `nohup` in the background,
   stdout and stderr redirected to the log, and exits 0 at once. Claude never waits on it.
3. `--push` touches `~/.cache/claude-sessions/push.pending`, then tries a non-blocking `flock` on
   `push.lock`. If another push holds the lock, it exits, because the holder will see the pending
   mark.
4. The holder waits until at least 60s have passed since the last successful push (stored in
   `push.stamp`). Then it repeats one step until `push.pending` is no older than when the step
   started: clear the pending mark, export, send. This trailing debounce means a burst of turns
   costs one push, and the last turn is never lost.
5. Errors go to `~/.cache/claude-sessions/push.log`, trimmed to the last 100 lines. Push always
   exits 0.

### Fetch (reader)

- `loadCachedRemote` reads `~/.cache/claude-sessions/remote.jsonl` and returns a `remoteMsg`
  stamped with the file's mtime.
- `fetchRemote` runs `ssh -o BatchMode=yes -o ConnectTimeout=5 macmini find
  ~/.cache/claude-sessions/hosts -name '*.jsonl' -exec cat {} +` with a 10s overall timeout. It
  writes the result to `remote.jsonl` (temp file plus rename) and returns a live `remoteMsg`. On
  the macmini it reads the directory directly.
- Both parse records off the UI thread. Bad lines are skipped.

### agents-configs changes

- `hooks/claude-sessions-push.sh` is the detaching wrapper from Push step 2. `install.sh`'s
  existing `link_dir hooks` links it into `~/.claude/hooks/`.
- `register_session_push` in `install.sh` adds Stop and SessionEnd entries running
  `~/.claude/hooks/claude-sessions-push.sh` (literal `~`, timeout 10) to the machine's
  `settings.json`. It uses the same jq merge as `register_env_guard`, so re-running adds nothing.
- `install_session_push_timer` in `install.sh` installs a 10-minute timer running the same script:
  `~/Library/LaunchAgents/com.grillermo.claude-sessions-push.plist`, loaded with
  `launchctl bootstrap`, on macOS; `claude-sessions-push.service` and `.timer` under
  `~/.config/systemd/user/`, enabled with `systemctl --user enable --now`, on Linux. It rewrites
  and reloads them only when their contents change.
- Rollout: commit in both repos, let git-sync carry them, then run `install.sh` once on each of the
  three hosts. During the plan, verify that the macmini's and pc's cswap account `settings.json`
  files are also symlinks to `~/.claude/settings.json`. If one isn't, register the hook there too.

## Error handling

- **Push** never blocks Claude and never prints. Failures go to `push.log`; the next hook or
  timer run retries.
- **Fetch** after a timeout, an unreachable macmini (away from home without WARP) or a non-zero
  exit: keep the cached rows and show `remote: cached <age>`. With no cache, there are no remote
  rows and no error screen.
- **Ingest** only rewrites a host file through temp file plus rename, under the lock. A crash
  leaves the previous files in place.
- **Remote resume** when the host is off: ssh prints its own error in the terminal, which
  claude-sessions has already handed over.
- **A missing host file** means that host has no rows.

## Testing

Go tests next to the existing ones, using temp directories and no network.
`CLAUDE_SESSIONS_REMOTE` points push and fetch at a local directory instead of
`ssh macmini`.

- Export: full-text extraction follows the existing message rules, and the parse cache is
  reused until the mtime or size changes.
- Ingest and cap: replacing a host's set, dropping exactly enough of the oldest sessions, always
  keeping the newest, tie order, a re-pushed dropped session staying dropped, and atomic rewrite.
- Push debounce: a pending mark set during a push causes one more round, and a second invocation
  exits while the lock is held.
- Reader: this host's rows are dropped, merge order is by mtime, the cursor stays on the same ID
  when rows arrive, bad lines are skipped, and the cached vs live status is right.
- Filter: matches text from the middle of a session, matches host names.
- `resumeCommand` for remote rows with and without cswap, quoting included.
- `bash -n` and shellcheck on the agents-configs hook and install functions, plus its existing
  `tests/install.test.sh` extended for idempotent registration.

Manual check: run `install.sh` on all three hosts. Start a session on the PC that mentions a unique
phrase only in the middle, end it, and wait a minute. On immunemac, search that phrase in
claude-sessions and resume the row; it should open on the PC in the right directory.

## Out of scope

- macaliada, mactastic and PC-windows.
- Showing where a full-text match occurred inside the preview. Rows match, but the preview still
  shows the first and last message.
- Bringing a session from another machine here to resume it locally.
