// Finding every account's transcripts.
//
// claude-swap (`cswap`) gives each logged-in account its own configuration
// directory under ~/.claude-swap-backup/sessions/<slot>-<email> and points
// Claude Code at it with CLAUDE_CONFIG_DIR. Each of those carries its own
// projects/ tree, so from inside one account the other accounts' sessions do
// not exist. The list reads all of them, plus the plain ~/.claude that predates
// the swapping, and remembers which account a session came from so it can be
// resumed as that account.
//
// Since claude-swap 1.0 the accounts share one chat history by default: an
// account's projects/ becomes a link to ~/.claude/projects (its own history is
// merged in on its next launch). Such an account adds nothing to read, and its
// sessions belong to no account in particular: any account can resume them, so
// they are listed once, as unmanaged ones, and resumed with plain claude.
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// swapDir is claude-swap's data directory, relative to the home directory.
const swapDir = ".claude-swap-backup"

// accountDirName matches one account's configuration directory: the slot
// number, then the address with its `@` flattened to an underscore.
var accountDirName = regexp.MustCompile(`^([0-9]+)-(.+)$`)

// account is the claude-swap profile a session belongs to. The zero value is
// the plain ~/.claude configuration, which claude-swap does not manage.
type account struct {
	number int
	email  string
}

// label is how an account is named on screen: empty for the unmanaged one,
// since there is nothing to distinguish it from.
func (a account) label() string {
	if a.number == 0 {
		return ""
	}
	return a.email
}

// source is one projects directory and the account that owns it.
type source struct {
	root    string
	account account
}

// discoverSources lists every projects directory to read: the unmanaged
// ~/.claude one, then one per claude-swap account. A machine without
// claude-swap has only the first, which is what the tool always used.
func discoverSources(home string) []source {
	sources := []source{{root: filepath.Join(home, ".claude", "projects")}}
	// Where each source really is, so an account whose projects/ is a link to
	// one already read (claude-swap's shared history) is skipped rather than
	// listing every session a second time under that account's name.
	read := map[string]bool{resolved(sources[0].root): true}

	accountsDir := filepath.Join(home, swapDir, "sessions")
	entries, err := os.ReadDir(accountsDir)
	if err != nil {
		return sources
	}
	emails := accountEmails(filepath.Join(home, swapDir, "sequence.json"))
	for _, entry := range entries {
		match := accountDirName.FindStringSubmatch(entry.Name())
		if match == nil {
			continue
		}
		number, err := strconv.Atoi(match[1])
		if err != nil || number == 0 {
			continue
		}
		email, known := emails[number]
		if !known {
			// Without the registry the directory name is the only clue, and it
			// holds the address with the first underscore standing in for `@`.
			email = strings.Replace(match[2], "_", "@", 1)
		}
		root := filepath.Join(accountsDir, entry.Name(), "projects")
		real := resolved(root)
		if read[real] {
			continue
		}
		read[real] = true
		sources = append(sources, source{
			root:    root,
			account: account{number: number, email: email},
		})
	}
	return sources
}

// resolved is path with every symlink followed, or path itself when it does
// not exist (yet), which still tells apart two directories that are not links.
func resolved(path string) string {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return real
	}
	return filepath.Clean(path)
}

// accountEmails reads the addresses claude-swap has on record, keyed by slot
// number. A missing or unreadable registry is not a problem: the directory
// names carry the same information, only less exactly.
func accountEmails(path string) map[int]string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var registry struct {
		Accounts map[string]struct {
			Email string `json:"email"`
		} `json:"accounts"`
	}
	if json.Unmarshal(data, &registry) != nil {
		return nil
	}
	emails := make(map[int]string, len(registry.Accounts))
	for slot, entry := range registry.Accounts {
		number, err := strconv.Atoi(slot)
		if err != nil || entry.Email == "" {
			continue
		}
		emails[number] = entry.Email
	}
	return emails
}
