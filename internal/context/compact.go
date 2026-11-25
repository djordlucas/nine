package ninectx

// Context compaction (adr/context-compaction.md), ported from seven's
// seven/agent/compact.py. When an agent whose context mode is "compact" nears
// the model's context window, the older part of its conversation is folded
// into a summary the model writes itself; the summary rides at the end of the
// system prompt and only the recent part is still sent message by message.
// Folded messages stay in the loop's history checkpoint — they are kept on
// disk, just no longer sent.

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strings"

	"nine/internal/llm"
)

// A request estimated above this share of the context window is compacted
// before it is sent.
const CompactAt = 0.75

// After compaction, the recent messages kept verbatim fit in this share of
// the window.
const KeepShare = 0.25

// One summarization request carries at most this share of the window of
// conversation, so summarizing a long history cannot itself overflow.
const ChunkShare = 0.5

// The summary's own budget, as a share of the window.
const SummaryShare = 0.08

// Longest piece of one message shown to the summarizer, in characters.
const MaxMessageChars = 4_000

// SummarizerPrompt is the summarizer's system prompt; also how a scripted
// model in tests recognises the call.
const SummarizerPrompt = `You summarize an AI agent's conversation so the agent can continue its work without the original messages.

Write to the agent, in the second person. Keep:
- every request and instruction it was given (by the user or another agent), verbatim when short, and who gave it;
- the task in progress, what is done, and the next step;
- decisions and the reasons for them;
- facts it will need again: file and database paths, tool names, run ids, agent ids, commands, errors and how they were fixed;
- what is still open or promised.

Drop small talk, superseded attempts, and tool output that is no longer needed. When given a summary so far, merge it with the new part into one summary. Use short sections and bullet lists.`

// SummaryLead is the user line that precedes the kept part of the history
// once a summary exists and the kept part does not start with a user message.
const SummaryLead = "(Earlier messages are summarized in the system prompt. Continue.)"

// SummarySection wraps the summary in the system prompt.
func SummarySection(summary string) string {
	if summary == "" {
		return ""
	}
	return "## Conversation summary (earlier messages were folded into this)\n\n" + summary
}

// RequestChars is the size of a request as sent, in characters.
func RequestChars(system string, messages []llm.Message, tools []llm.ToolDef) int {
	n := len(system)
	for _, m := range messages {
		b, _ := json.Marshal(m)
		n += len(b)
	}
	for _, t := range tools {
		b, _ := json.Marshal(t)
		n += len(b)
	}
	return n
}

// EstimateTokensFromChars turns a character count into the same token
// estimate the builder uses for budgeting.
func EstimateTokensFromChars(chars int) int {
	return (chars*bytesPerTokenNum + bytesPerTokenDen - 1) / bytesPerTokenDen
}

// CutIndex is where to cut a conversation: the index of the first message
// kept. The kept part starts at a user or assistant message — never a message
// carrying tool results, whose calls would be folded away — preferably at a
// user message, and fits in keepChars when it can; otherwise it is the
// shortest such part. 0 means nothing can be folded.
func CutIndex(messages []llm.Message, keepChars int) int {
	size := 0
	atUser := 0
	atAny := 0
	last := 0
	for i := len(messages) - 1; i > 0; i-- {
		b, _ := json.Marshal(messages[i])
		size += len(b)
		// A user message that carries tool results answers an assistant tool
		// call: cutting there orphans the result, which the API rejects.
		if len(messages[i].ToolResults) > 0 {
			continue
		}
		if last == 0 {
			last = i
		}
		if size > keepChars {
			break
		}
		atAny = i
		if messages[i].Role == "user" {
			atUser = i
		}
	}
	if atUser != 0 {
		return atUser
	}
	if atAny != 0 {
		return atAny
	}
	return last
}

var overflowRe = regexp.MustCompile(`\b(400|413|422)\b.*(context|token|too (long|large)|length|exceed)`)
var overflowRe2 = regexp.MustCompile(`(context|token|too (long|large)|length|exceed).*\b(400|413|422)\b`)

// IsOverflow reports whether err is the model refusing an over-long request.
func IsOverflow(err error) bool {
	if err == nil {
		return false
	}
	text := err.Error()
	return overflowRe.MatchString(text) || overflowRe2.MatchString(text) ||
		strings.Contains(strings.ToLower(text), "context length exceeded") ||
		strings.Contains(strings.ToLower(text), "prompt is too long")
}

// Render is one message as the summarizer reads it.
func Render(m llm.Message) string {
	switch m.Role {
	case "user":
		if len(m.ToolResults) > 0 {
			parts := make([]string, 0, len(m.ToolResults))
			for _, tr := range m.ToolResults {
				parts = append(parts, fmt.Sprintf("[result of tool call %s]\n%s", tr.ToolCallID, clip(tr.Content)))
			}
			return strings.Join(parts, "\n\n")
		}
		return "[user]\n" + clip(m.Text)
	default: // assistant
		parts := []string{"[assistant]"}
		if m.Text != "" {
			parts = append(parts, clip(m.Text))
		}
		for _, c := range m.ToolCalls {
			parts = append(parts, fmt.Sprintf("→ %s(%s)", c.Name, clip(string(c.Input), 1_000)))
		}
		return strings.Join(parts, "\n")
	}
}

// Summarize folds messages into previous (a summary of what came before
// them, or ""), in chunks that each fit chunkChars, each chunk's summary
// becoming the previous summary of the next. submit is the LLM call path
// (the loop's queue); priority is the queue priority to run it at.
func Summarize(
	ctx context.Context,
	submit func(ctx context.Context, priority int, req llm.Request) (llm.Response, error),
	priority int,
	previous string,
	messages []llm.Message,
	windowTokens int,
) (string, error) {
	chunkChars := EstimateTokensToChars(int(float64(windowTokens) * ChunkShare))
	maxTokens := max(256, int(math.Floor(float64(windowTokens)*SummaryShare)))
	summary := previous
	if len(messages) == 0 {
		if summary == "" {
			return "", fmt.Errorf("nothing to summarize")
		}
		return summary, nil
	}
	for _, chunk := range chunks(renderAll(messages), chunkChars) {
		prompt := ""
		if summary != "" {
			prompt = "The summary so far:\n\n" + summary + "\n\n"
		}
		prompt += "The conversation" + map[bool]string{true: " since then", false: ""}[summary != ""] + ":\n\n" + chunk +
			"\n\nWrite the " + map[bool]string{true: "merged ", false: ""}[summary != ""] +
			fmt.Sprintf("summary, in at most %d words.", maxTokens*6/10)
		resp, err := submit(ctx, priority, llm.Request{
			System:    SummarizerPrompt,
			Messages:  []llm.Message{{Role: "user", Text: prompt}},
			MaxTokens: maxTokens,
		})
		if err != nil {
			return "", err
		}
		text := strings.TrimSpace(resp.Text)
		if text == "" {
			return "", fmt.Errorf("the summarizer returned an empty summary")
		}
		summary = text
	}
	return summary, nil
}

// EstimateTokensToChars is the inverse of EstimateTokens, rounded down.
func EstimateTokensToChars(tokens int) int {
	return tokens * bytesPerTokenDen / bytesPerTokenNum
}

func renderAll(messages []llm.Message) []string {
	out := make([]string, 0, len(messages))
	for _, m := range messages {
		out = append(out, Render(m))
	}
	return out
}

// chunks groups whole rendered messages, each group at most maxChars (a
// longer message is a group alone).
func chunks(parts []string, maxChars int) []string {
	out := make([]string, 0, len(parts))
	cur := ""
	for _, p := range parts {
		if cur != "" && len(cur)+len(p)+2 > maxChars {
			out = append(out, cur)
			cur = ""
		}
		if cur != "" {
			cur += "\n\n" + p
		} else {
			cur = p
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

// clip keeps the start and end of a long text, cutting the middle.
func clip(s string, maxChars ...int) string {
	max := MaxMessageChars
	if len(maxChars) > 0 {
		max = maxChars[0]
	}
	if len(s) <= max {
		return s
	}
	half := max / 2
	return fmt.Sprintf("%s\n… [%d characters cut] …\n%s", s[:half], len(s)-max, s[len(s)-half:])
}
