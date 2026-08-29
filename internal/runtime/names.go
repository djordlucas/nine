package runtime

import (
	"strings"
)

// nameFromPrompt derives a short slug from the first ~5 words of a prompt.
// Returns an empty string if text is blank.
func nameFromPrompt(text string) string {
	log.Debug("nameFromPrompt", "text", text)
	words := strings.Fields(text)
	const maxWords = 5
	if len(words) > maxWords {
		words = words[:maxWords]
	}
	parts := make([]string, 0, len(words))
	for _, w := range words {
		var b strings.Builder
		for _, c := range strings.ToLower(w) {
			if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' {
				b.WriteRune(c)
			}
		}
		if s := b.String(); s != "" {
			parts = append(parts, s)
		}
	}
	name := strings.Join(parts, "-")
	const maxLen = 32
	if len(name) > maxLen {
		name = name[:maxLen]
	}
	log.Debug("nameFromPrompt result", "name", name)
	return name
}
