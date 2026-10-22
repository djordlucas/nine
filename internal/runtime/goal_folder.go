package runtime

import (
	"strings"
	"unicode"
)

// A goal's folder (docs/goal-sessions.md): one directory in the workspace for
// everything a goal produces, named in each of its turns, so a later turn finds
// what an earlier one wrote instead of writing it again elsewhere. A goal
// session was seen writing the same daily report to the workspace root,
// reports/, self/updates/ and workspace/workspace/ in turn.

// goalFolderSlugLen bounds the description part of a goal's folder name.
const goalFolderSlugLen = 40

// goalFolderIDLen is the longest id a folder name keeps whole.
const goalFolderIDLen = 16

// GoalFolder is the workspace folder for a goal's output: goals/, then a slug
// of its description and the start of its id, which keeps two goals with the
// same words apart. It ends in a slash.
func GoalFolder(id, description string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(description) {
		switch {
		case r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(r)
		default:
			dash = true
		}
		if b.Len() >= goalFolderSlugLen {
			break
		}
	}
	slug := strings.TrimRight(b.String(), "-")
	if len(slug) > goalFolderSlugLen {
		slug = strings.TrimRight(slug[:goalFolderSlugLen], "-")
	}
	// A generated id is a UUID, whose first 8 characters are enough; a name a
	// [[process]] block gave its goal is kept whole.
	short := id
	if len(short) > goalFolderIDLen {
		short = short[:8]
	}
	if slug == "" {
		return "goals/" + short + "/"
	}
	return "goals/" + slug + "-" + short + "/"
}
