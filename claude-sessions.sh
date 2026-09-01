# shellcheck shell=bash
# Shell function for claude-sessions: land in the session's directory.
#
# Source this from your ~/.zshrc or ~/.bashrc:
#
#     source ~/c/claude-sessions/claude-sessions.sh
#
# The TUI runs as a child process, so its `cd` cannot reach this shell. Instead
# it writes the chosen session's directory to a file we hand it, and once Claude
# exits we cd there ourselves. Works in zsh and bash; the _cs_ variables are
# this function's scratch space.
claude-sessions() {
    _cs_file=$(mktemp "${TMPDIR:-/tmp}/claude-sessions.XXXXXX") || return 1

    CLAUDE_SESSIONS_CWD_FILE="$_cs_file" command claude-sessions "$@"
    _cs_status=$?

    _cs_dir=$(cat "$_cs_file" 2>/dev/null)
    rm -f "$_cs_file"

    # Quitting the TUI leaves the file empty; only move for a real choice.
    if [ -n "$_cs_dir" ] && [ -d "$_cs_dir" ]; then
        cd "$_cs_dir" || true
    fi

    return $_cs_status
}
