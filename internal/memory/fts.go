package memory

import (
	"strings"
	"unicode"
)

// ftsTerm is one positive search term and how it joins to the one before it.
type ftsTerm struct {
	text string
	or   bool // joined with OR rather than AND
}

// rawTok is one whitespace- or quote-delimited token from the user's query.
type rawTok struct {
	text   string
	quoted bool
}

// ftsQuery translates free-form user input into an FTS5 MATCH expression that
// cannot raise a syntax error.
//
// This is the compatibility shim for the single biggest behavioural difference
// between FTS5 and the Postgres full-text search it replaces: the old
// websearch_to_tsquery accepted literally any string, while FTS5's MATCH parser
// rejects a stray quote, an unbalanced paren, a trailing AND, a bare NEAR, a
// leading '*', or a column filter naming a column that does not exist. A model
// composing a search query produces all of those, and under Postgres they were
// simply impossible to hit.
//
// The rules mirror the old websearch behaviour:
//
//	bare words       AND-ed together
//	"quoted phrase"  kept as a single phrase
//	or / OR          joins the terms on either side with OR
//	-word            excluded
//
// Every term is emitted as a double-quoted FTS5 string literal, which makes its
// contents inert — inside a string literal FTS5 runs the tokenizer and treats
// the result as a phrase, so operators, punctuation, '*', ':', '^', parens and
// reserved words are all just text. That single decision closes the entire
// syntax-error class, rather than blacklisting characters one at a time.
//
// Returns "" when nothing searchable is left; the caller reports no results
// instead of running the query, because MATCH ” is itself a syntax error.
func ftsQuery(raw string) string {
	terms, negs := parseWebSearch(raw)
	if len(terms) == 0 {
		// A query of pure negations has no meaning in websearch either, and FTS5's
		// NOT is binary — there would be nothing to subtract from.
		return ""
	}

	var b strings.Builder
	for i, t := range terms {
		if i > 0 {
			if t.or {
				b.WriteString(" OR ")
			} else {
				b.WriteString(" AND ")
			}
		}
		b.WriteString(quoteFTS(t.text))
	}
	for _, n := range negs {
		b.WriteString(" NOT ")
		b.WriteString(quoteFTS(n))
	}
	return b.String()
}

// quoteFTS renders s as an FTS5 string literal. Doubling an embedded quote is
// the only escaping FTS5 string literals have.
//
// scanTokens consumes every quote as a phrase delimiter, so the terms reaching
// here never actually contain one — the doubling is what keeps quoteFTS safe on
// its own terms rather than only in combination with its current caller.
func quoteFTS(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// searchable reports whether s contains anything the tokenizer will index. A
// term of pure punctuation would produce the empty phrase "" — a syntax error —
// so it is dropped before it ever reaches the parser.
func searchable(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

// scanTokens splits raw into quote- and whitespace-delimited tokens. An
// unterminated quote ends at end-of-input rather than being an error, which is
// the whole point: there is no input this can reject.
func scanTokens(raw string) []rawTok {
	var (
		toks []rawTok
		cur  strings.Builder
		inQ  bool
		has  bool
	)
	emit := func(quoted bool) {
		if has || quoted {
			toks = append(toks, rawTok{text: cur.String(), quoted: quoted})
		}
		cur.Reset()
		has = false
	}
	for _, r := range raw {
		switch {
		case r == '"':
			emit(inQ)
			inQ = !inQ
		case !inQ && unicode.IsSpace(r):
			emit(false)
		default:
			cur.WriteRune(r)
			has = true
		}
	}
	emit(inQ)
	return toks
}

// parseWebSearch interprets scanned tokens as positive terms and exclusions.
func parseWebSearch(raw string) (terms []ftsTerm, negs []string) {
	nextOr := false
	for _, t := range scanTokens(raw) {
		text := t.text

		// A bare `or` joins the terms around it and is not itself a term. A
		// quoted "or" is a search for the word, and a leading or trailing `or`
		// has nothing to join, so it is dropped.
		if !t.quoted && strings.EqualFold(text, "or") {
			if len(terms) > 0 {
				nextOr = true
			}
			continue
		}

		negated := false
		if !t.quoted && strings.HasPrefix(text, "-") {
			negated = true
			text = strings.TrimLeft(text, "-")
		}
		if !searchable(text) {
			continue
		}
		if negated {
			negs = append(negs, text)
			continue
		}
		terms = append(terms, ftsTerm{text: text, or: nextOr})
		nextOr = false
	}
	return terms, negs
}
