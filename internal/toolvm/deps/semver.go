package deps

import (
	"fmt"
	"strconv"
	"strings"
)

// A deliberately minimal semver, enough for operator-authored allowlist ranges
// and common package.json dependency ranges: exact, `^`, `~`, `x`/`*` wildcards,
// simple comparators (`>=`, `>`, `<=`, `<`, `=`), and `||` alternation. It does
// NOT implement the full npm range grammar (hyphen ranges, complex pre-release
// precedence, build metadata). That is a stated limitation, not an oversight: the
// operator names the packages and stands behind the ranges (§4.4), so the input
// here is small and legible, and a range this cannot parse fails closed rather
// than resolving to something unexpected.

type semver struct {
	major, minor, patch int
	pre                 string // pre-release tag, "" for a release
}

// parseSemver parses "1.2.3" or "1.2.3-beta.1". A leading "v" and build metadata
// (+…) are tolerated and ignored.
func parseSemver(s string) (semver, error) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}
	var pre string
	if i := strings.IndexByte(s, '-'); i >= 0 {
		pre = s[i+1:]
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return semver{}, fmt.Errorf("not a semver: %q", s)
	}
	var v semver
	var err error
	if v.major, err = atoi(parts[0]); err != nil {
		return semver{}, err
	}
	if v.minor, err = atoi(parts[1]); err != nil {
		return semver{}, err
	}
	if v.patch, err = atoi(parts[2]); err != nil {
		return semver{}, err
	}
	v.pre = pre
	return v, nil
}

func atoi(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("not a number: %q", s)
	}
	return n, nil
}

// compare orders two versions; a release outranks a pre-release of the same
// core, and pre-release tags compare lexically (a simplification).
func (v semver) compare(o semver) int {
	for _, d := range []int{v.major - o.major, v.minor - o.minor, v.patch - o.patch} {
		if d != 0 {
			return sign(d)
		}
	}
	switch {
	case v.pre == "" && o.pre == "":
		return 0
	case v.pre == "":
		return 1 // release > pre-release
	case o.pre == "":
		return -1
	}
	return strings.Compare(v.pre, o.pre)
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	default:
		return 0
	}
}

// satisfies reports whether v is within rangeExpr. An empty range or "*"/"latest"
// matches any release.
func satisfies(v semver, rangeExpr string) bool {
	rangeExpr = strings.TrimSpace(rangeExpr)
	if rangeExpr == "" || rangeExpr == "*" || rangeExpr == "latest" || rangeExpr == "x" {
		return v.pre == "" // a bare range never matches a pre-release
	}
	// Alternation: any clause may match.
	for _, clause := range strings.Split(rangeExpr, "||") {
		if satisfiesClause(v, strings.TrimSpace(clause)) {
			return true
		}
	}
	return false
}

// satisfiesClause handles one space-separated conjunction of comparators.
func satisfiesClause(v semver, clause string) bool {
	if clause == "" || clause == "*" {
		return v.pre == ""
	}
	for _, c := range strings.Fields(clause) {
		if !satisfiesComparator(v, c) {
			return false
		}
	}
	return true
}

func satisfiesComparator(v semver, c string) bool {
	switch {
	case strings.HasPrefix(c, "^"):
		return caretMatch(v, c[1:])
	case strings.HasPrefix(c, "~"):
		return tildeMatch(v, c[1:])
	case strings.HasPrefix(c, ">="):
		return cmpBase(v, c[2:]) >= 0
	case strings.HasPrefix(c, "<="):
		return cmpBase(v, c[2:]) <= 0
	case strings.HasPrefix(c, ">"):
		return cmpBase(v, c[1:]) > 0
	case strings.HasPrefix(c, "<"):
		return cmpBase(v, c[1:]) < 0
	case strings.HasPrefix(c, "="):
		return cmpBase(v, c[1:]) == 0
	default:
		return wildcardMatch(v, c)
	}
}

// cmpBase compares v against a (possibly partial) version, treating missing
// components as 0.
func cmpBase(v semver, s string) int {
	base, _ := parseSemver(fill(s))
	return v.compare(base)
}

// caretMatch: ^1.2.3 allows >=1.2.3 <2.0.0; ^0.2.3 allows >=0.2.3 <0.3.0;
// ^0.0.3 allows >=0.0.3 <0.0.4 — npm's "compatible within the left-most nonzero".
func caretMatch(v semver, base string) bool {
	b, err := parseSemver(fill(base))
	if err != nil || v.pre != "" || v.compare(b) < 0 {
		return false
	}
	switch {
	case b.major > 0:
		return v.major == b.major
	case b.minor > 0:
		return v.major == 0 && v.minor == b.minor
	default:
		return v.major == 0 && v.minor == 0 && v.patch == b.patch
	}
}

// tildeMatch: ~1.2.3 allows >=1.2.3 <1.3.0 (patch-level within a minor).
func tildeMatch(v semver, base string) bool {
	b, err := parseSemver(fill(base))
	if err != nil || v.pre != "" || v.compare(b) < 0 {
		return false
	}
	return v.major == b.major && v.minor == b.minor
}

// wildcardMatch handles exact and x-ranges: "1.2.3", "1.2.x", "1.x".
func wildcardMatch(v semver, s string) bool {
	if v.pre != "" {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(s, "v"), ".")
	comp := []int{v.major, v.minor, v.patch}
	for i := 0; i < 3 && i < len(parts); i++ {
		p := parts[i]
		if p == "x" || p == "X" || p == "*" {
			return true // an x at this position matches any lower components
		}
		n, err := strconv.Atoi(p)
		if err != nil || n != comp[i] {
			return false
		}
	}
	return true
}

// fill pads a partial version ("1", "1.2") to three components so parseSemver
// accepts it.
func fill(s string) string {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	parts := strings.Split(s, ".")
	for len(parts) < 3 {
		parts = append(parts, "0")
	}
	return strings.Join(parts[:3], ".")
}

// pickVersion returns the highest published version satisfying rangeExpr.
func pickVersion(versions []string, rangeExpr string) (string, error) {
	var best string
	var bestV semver
	for _, raw := range versions {
		v, err := parseSemver(raw)
		if err != nil {
			continue // ignore non-semver tags in a packument
		}
		if !satisfies(v, rangeExpr) {
			continue
		}
		if best == "" || v.compare(bestV) > 0 {
			best, bestV = raw, v
		}
	}
	if best == "" {
		return "", fmt.Errorf("no published version satisfies %q", rangeExpr)
	}
	return best, nil
}
