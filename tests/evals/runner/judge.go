package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"nine/internal/llm"
)

// LLM-as-judge (docs/evals.md §7). Used only for open-ended answers where
// side-effect/trajectory/regex checks can't apply, and kept a minority of cases.
// The judge must be a stronger, different model than the one under test; it is
// asked for a structured {score, reason} against an explicit rubric and passes
// when score >= the case's pass_score.

// NewJudge returns a JudgeFunc that scores an answer with a fresh provider built
// per case from the case's judge model. providerFor lets tests inject a scripted
// judge; pass ProviderFor for live use.
func NewJudge(providerFor func(model string) (llm.Provider, error)) JudgeFunc {
	return func(c *Case, answer string) (bool, string, error) {
		j := c.Expect.Answer.Judge
		if j == nil {
			return true, "", nil // no judge declared
		}
		provider, err := providerFor(j.Model)
		if err != nil {
			return false, "", fmt.Errorf("judge provider: %w", err)
		}
		score, reason, err := scoreAnswer(context.Background(), provider, j.Rubric, answer)
		if err != nil {
			return false, "", err
		}
		if score < j.PassScore {
			return false, fmt.Sprintf("score %.2f < %.2f: %s", score, j.PassScore, reason), nil
		}
		return true, fmt.Sprintf("score %.2f: %s", score, reason), nil
	}
}

const judgeSystem = `You are a strict evaluator. Given a rubric and a candidate answer, decide how well the answer satisfies the rubric. Respond with ONLY a JSON object of the form {"score": <0.0-1.0>, "reason": "<short justification>"}. Do not include any other text.`

// scoreAnswer asks provider to grade answer against rubric and parses the
// structured {score, reason} verdict.
func scoreAnswer(ctx context.Context, provider llm.Provider, rubric, answer string) (float64, string, error) {
	prompt := fmt.Sprintf("Rubric:\n%s\n\nCandidate answer:\n%s\n\nReturn the JSON verdict now.", rubric, answer)
	resp, err := provider.Complete(ctx, llm.Request{
		System:    judgeSystem,
		Messages:  []llm.Message{{Role: "user", Text: prompt}},
		MaxTokens: 512,
	})
	if err != nil {
		return 0, "", fmt.Errorf("judge complete: %w", err)
	}
	return parseVerdict(resp.Text)
}

// verdictJSON matches the first {...} object in the model's reply, tolerating
// stray prose around it (weaker judges wrap the JSON in text).
var verdictJSON = regexp.MustCompile(`(?s)\{.*\}`)

func parseVerdict(text string) (float64, string, error) {
	m := verdictJSON.FindString(text)
	if m == "" {
		return 0, "", fmt.Errorf("judge returned no JSON: %q", strings.TrimSpace(text))
	}
	var v struct {
		Score  float64 `json:"score"`
		Reason string  `json:"reason"`
	}
	if err := json.Unmarshal([]byte(m), &v); err != nil {
		return 0, "", fmt.Errorf("judge JSON %q: %w", m, err)
	}
	return v.Score, v.Reason, nil
}
