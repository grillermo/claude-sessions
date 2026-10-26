# claude-sessions

A terminal UI over your recent Claude Code sessions, across every account you
are logged in to. It lists the latest transcripts, shows what each one started
with and where it got to, and resumes the one you pick — as the account it
belongs to — in the terminal you launched it from.

```
$ claude-sessions
```

Give it a directory and the list holds only sessions that ran there or
anywhere below it — a whole tree of repos, or one of them:

```
$ claude-sessions ~/c/datacenters/
```

The directory is scoped out before the newest-40 cut, so you get that
project's newest sessions rather than whatever of it survived a global one.
The title line names the directory the list is limited to.

## Accounts

[claude-swap](https://github.com/grillermo/claude-swap) gives every logged-in
account its own configuration directory, so from inside one account the other
accounts' sessions do not exist. The list reads all of them — every
`~/.claude-swap-backup/sessions/<slot>-<email>/projects`, plus the plain
`~/.claude/projects` that predates the swapping — and merges them into one
timeline, newest first, rather than a run of each account.

A row belonging to an account is labelled with its address next to the project
path, and the filter matches that too, so `/work.com` narrows the list to one
account. Rows with no label are the unmanaged `~/.claude`.

## The list

Each row is one session: the project it ran in, the account it belongs to, and
its age, then two columns —
the first message you typed on the left, the last message of the conversation
on the right. Rows are three lines tall, so a screenful holds many sessions.

| Key | |
| --- | --- |
| `↑` `↓` (`k` `j`) | move between sessions |
| `tab` | switch to the full-screen preview and back |
| `enter` | resume the selected session in this terminal |
| `/` | filter as you type; `esc` leaves the filter |
| `q` | quit |

Filtering matches the first message, the last message, and the project path.
It stays live behind the preview, so you can narrow and read without leaving.

## What counts as a message

Transcripts hold far more than conversation. Only user and assistant prose is
shown: tool calls, tool results, thinking blocks, meta records, and
`<system-reminder>` blocks never appear. Slash commands are rendered as the
line you typed, and a session opening with an argument-less command such as
`/clear` shows the next real message instead, since `/clear` says nothing about
what the session was about.

## Resuming

`enter` execs `cd <project> && claude --resume <id>` over the TUI's own
process, so the session takes over the terminal with no wrapper left behind.

A session belonging to a claude-swap account is resumed through
`cswap run <slot> -- --resume <id>` instead, which points Claude at that
account's configuration and credentials for that terminal only. Plain
`claude --resume` would look for the transcript under whichever account is
current and not find it.

## Staying in the session's directory

That `cd` happens in a child of your shell, so on its own it dies with the
session: quit Claude and you are back where you typed `claude-sessions`. To
land in the project instead, source the shell function and use that:

```sh
source ~/c/claude-sessions/claude-sessions.sh
```

It hands the binary a scratch file through `CLAUDE_SESSIONS_CWD_FILE`, the
binary writes the chosen project path there before exec'ing Claude, and the
function `cd`s your real shell there once the session ends. Quitting the TUI
without picking anything leaves you where you were, and Claude's exit status
still comes through. Without the function the binary works exactly as before.

## Other machines

The list also holds the sessions of the other machines: immunemac, the
macmini and the PC. Each machine pushes an index of its sessions to the
macmini, holding each session's first and last message, all of its prose and
where it ran, and every launch reads a mirror of the macmini's copy. Those rows
carry the machine's name next to the project path, and the filter matches it,
so `/pc` narrows the list to the PC. The filter matches the full prose of
every session, yours and Claude's, on every machine.

Startup does not wait for the network. The mirror is read as it is, and an
`rsync` in the background brings it up to date (it gives up after 10 minutes
and clears any half-written temporary files before the next try); the title
says `remote: live` once it has, or `remote: cached 3h ago` when the macmini is
out of reach. A machine with no transcripts of its own still starts the TUI as
long as a remote source exists.

`enter` on another machine's session resumes it there:
`ssh -t -- <host>` runs the same `cd … && claude --resume` (or `cswap run`) in
that machine's login shell.

Pushes come from a Claude Code hook after every turn and at the end of every
session, and from a 10-minute timer, all installed by `~/c/agents-configs`
(`hooks/claude-sessions-push.sh`). A push sends only the sessions that changed,
at most one goes out a minute, and an ssh push is cut off after 2 minutes.
`claude-sessions --push` runs one by hand;
`~/.cache/claude-sessions/push.log` holds the last errors.

The index covers the newest 5 GB of transcripts across all machines. Past
that, the oldest sessions drop out of the index to make room; their transcripts
stay on disk.

## Running it

The `claude-sessions` script builds the binary into `bin/` on first run and
whenever a source file changes, then execs it. Go is needed for that build and
for nothing else afterwards. Put the repo on your `PATH`:

```sh
export PATH="$HOME/c/claude-sessions:$PATH"
```

Transcripts are read from every account's projects directory, as above.
`CLAUDE_PROJECTS_DIR` overrides all of that with one directory, and sessions
found there are resumed with plain `claude --resume`.

## Tests

```sh
go test ./...
```
