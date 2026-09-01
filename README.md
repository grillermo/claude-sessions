# claude-sessions

A terminal UI over your recent Claude Code sessions. It lists the latest
transcripts, shows what each one started with and where it got to, and resumes
the one you pick in the terminal you launched it from.

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

## The list

Each row is one session: the project it ran in and its age, then two columns —
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

## Running it

The `claude-sessions` script builds the binary into `bin/` on first run and
whenever a source file changes, then execs it. Go is needed for that build and
for nothing else afterwards. Put the repo on your `PATH`:

```sh
export PATH="$HOME/c/claude-sessions:$PATH"
```

Transcripts are read from `~/.claude/projects`; `CLAUDE_PROJECTS_DIR`
overrides that.

## Tests

```sh
go test ./...
```
