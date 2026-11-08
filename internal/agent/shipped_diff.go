package agent

import (
	"strings"
)

// diffHunks is the Go port of the sandbox's nine:diff hunks(): a line-level LCS
// diff rendered as unified-diff hunks. added/removed count the changed lines;
// text is the unified diff body; truncated marks a capped diff.
type diffResult struct {
	added     int
	removed   int
	text      string
	truncated bool
}

type diffOp struct {
	kind byte // ' ' context, '-' removed, '+' added
	line string
}

// diffHunks computes the unified diff of a and b.
func diffHunks(a, b string) diffResult {
	const maxLines = 200_000 // a 50 MB file cannot be diffed into a chat message anyway
	al := strings.Split(a, "\n")
	bl := strings.Split(b, "\n")
	trunc := len(al) > maxLines || len(bl) > maxLines
	if trunc {
		if len(al) > maxLines {
			al = al[:maxLines]
		}
		if len(bl) > maxLines {
			bl = bl[:maxLines]
		}
	}

	// LCS dynamic programming with a memory guard: the table is O(n*m).
	const maxCells = 64 << 20
	if len(al)*len(bl) > maxCells {
		return diffResult{added: len(bl), removed: len(al), text: fallbackDiff(al, bl), truncated: true}
	}

	n, m := len(al), len(bl)
	lcs := make([][]int32, n+1)
	for i := range lcs {
		lcs[i] = make([]int32, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if al[i] == bl[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}

	var ops []diffOp
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case al[i] == bl[j]:
			ops = append(ops, diffOp{' ', al[i]})
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			ops = append(ops, diffOp{'-', al[i]})
			i++
		default:
			ops = append(ops, diffOp{'+', bl[j]})
			j++
		}
	}
	for i < n {
		ops = append(ops, diffOp{'-', al[i]})
		i++
	}
	for j < m {
		ops = append(ops, diffOp{'+', bl[j]})
		j++
	}

	const ctx = 3
	var sb strings.Builder
	added, removed := 0, 0
	for _, h := range splitHunks(ops, ctx) {
		for _, o := range h {
			switch o.kind {
			case '+':
				added++
				sb.WriteString("+" + o.line + "\n")
			case '-':
				removed++
				sb.WriteString("-" + o.line + "\n")
			default:
				sb.WriteString(" " + o.line + "\n")
			}
		}
		sb.WriteString("\n")
	}
	return diffResult{added: added, removed: removed, text: strings.TrimRight(sb.String(), "\n"), truncated: trunc}
}

func fallbackDiff(a, b []string) string {
	var sb strings.Builder
	for _, l := range a {
		sb.WriteString("-" + l + "\n")
	}
	for _, l := range b {
		sb.WriteString("+" + l + "\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}

// splitHunks groups ops into runs of changes with ctx lines of surrounding
// context merged in.
func splitHunks(ops []diffOp, ctx int) [][]diffOp {
	changed := make([]bool, len(ops))
	for i, o := range ops {
		changed[i] = o.kind != ' '
	}
	var hunks [][]diffOp
	i := 0
	for i < len(ops) {
		if !changed[i] {
			i++
			continue
		}
		start := max(0, i-ctx)
		j := i
		for j < len(ops) && changed[j] {
			j++
		}
		// extend through context to absorb a following change within ctx lines
		for {
			next := -1
			for k := j; k < min(len(ops), j+ctx+1); k++ {
				if changed[k] {
					next = k
					break
				}
			}
			if next < 0 {
				break
			}
			for j < next {
				j++
			}
		}
		end := min(len(ops), j+ctx)
		hunks = append(hunks, ops[start:end])
		i = end
	}
	return hunks
}
