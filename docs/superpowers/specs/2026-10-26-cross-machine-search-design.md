Status: DRAFT

# Cross-machine search for claude-sessions

## Goal

List, search and resume Claude Code sessions from immunemac, macmini and PC-linux in one
claude-sessions timeline, without slowing startup. Rows from other machines may arrive a moment
after launch.

## Decisions

- **Hosts in scope: immunemac, macmini, PC-linux.** macaliada, mactastic and PC-windows stay out
  for now: macaliada has no Go on its non-login PATH, and mactastic has no sessions.
- **Architecture: each host pushes a summary index to the macmini (option C).** The macmini is
  always on and the only host reachable over WARP, so one fetch from it covers every host, and a
  sleeping host stays searchable at its last push.
- **Startup: cached remote rows show at once, refreshed in the background.** Startup must stay
  fast, and async updates are acceptable.
- **Search depth: every prompt and every Claude answer (all prose), not just first/last.** This
  replaces the earlier "first + last + prompts capped at 4 KB" choice. Measured prose today: about
  9 MB across the three hosts (immunemac 5.4, macmini 3.1, pc 0.6), so it fits in memory.
- **Cap: the newest 1000 MB of history, oldest sessions purged first.** Measured on raw transcript
  bytes across all hosts (846 MB today, so nothing is purged yet). Purging only removes sessions
  from the shared index on the macmini. Transcripts on disk are never deleted. (The basis and the
  purge target are my defaults; the user has not confirmed them yet.) User: "when the cap is
  reached, enough old conversations should be dropped to make space for new ones." The cap is a
  rolling window. After each push, sessions are dropped oldest-mtime first, only as many as it
  takes to get the total back under 1000 MB, so the newest sessions always fit.
- **Subagent transcripts are not indexed.** Their output already returns to the parent session.
  (My default; the user has not confirmed it yet.)
- **Resume of a row from another machine: `ssh -t` to that host.** That host has the working tree,
  the transcript and the cswap account.
- **Push trigger: a debounced Stop/SessionEnd hook plus a 10-minute timer.** The hook keeps the
  index fresh, and the timer catches sessions that end abnormally.
- **The push hook and timer ship through `~/c/agents-configs`, not an installer in this repo.**
  User: "take advantage of the fact that I share my claude settings across all the machines
  with ../agents-configs". The hook script goes in `agents-configs/hooks/`, which `install.sh`
  already symlinks into `~/.claude/hooks/` on every host. `install.sh` registers the Stop and
  SessionEnd entries in each machine-local `settings.json` with the same idempotent jq merge as
  `register_env_guard`, and installs the launchd or systemd timer. git-sync carries the change,
  and re-running `install.sh` on each host activates it. Checked: the macmini and pc both have the
  hooks symlinks, jq, and the env guard registered.
