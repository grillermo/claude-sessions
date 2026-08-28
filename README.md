# claude-sessions

A terminal UI over your recent Claude Code sessions. It lists the latest
transcripts, shows what each one started with and where it got to, and resumes
the one you pick in the terminal you launched it from.

```
$ claude-sessions
```

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
