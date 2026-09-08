// Finding every account's transcripts.
//
// claude-swap (`cswap`) gives each logged-in account its own configuration
// directory under ~/.claude-swap-backup/sessions/<slot>-<email> and points
// Claude Code at it with CLAUDE_CONFIG_DIR. Each of those carries its own
// projects/ tree, so from inside one account the other accounts' sessions do
// not exist. The list reads all of them, plus the plain ~/.claude that predates
// the swapping, and remembers which account a session came from so it can be
// resumed as that account.
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
		sources = append(sources, source{
			root:    filepath.Join(accountsDir, entry.Name(), "projects"),
			account: account{number: number, email: email},
		})
	}
	return sources
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
