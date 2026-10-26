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
- **Search depth: first + last + every user prompt, capped at about 4 KB per session.** This makes
  topics from the middle of a session findable.
- **Resume of a row from another machine: `ssh -t` to that host.** That host has the working tree,
  the transcript and the cswap account.
- **Push trigger: a debounced Stop/SessionEnd hook plus a 10-minute timer.** The hook keeps the
  index fresh, and the timer catches sessions that end abnormally.
