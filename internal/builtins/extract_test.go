package builtins

import (
	"strings"
	"testing"
)

func TestExtractTextStripsTagsAndScript(t *testing.T) {
	input := `<html><head>
<script>alert('noise')</script>
<style>body{color:red}</style>
</head><body><p>text</p></body></html>`

	got := extractText(strings.NewReader(input))

	if strings.Contains(got, "alert") {
		t.Errorf("output contains script content: %q", got)
	}
	if strings.Contains(got, "color:red") {
		t.Errorf("output contains style content: %q", got)
	}
	if !strings.Contains(got, "text") {
		t.Errorf("output missing paragraph text: %q", got)
	}
}

func TestExtractTextPlainText(t *testing.T) {
	input := "hello world"
	got := extractText(strings.NewReader(input))
	if !strings.Contains(got, "hello") || !strings.Contains(got, "world") {
		t.Errorf("plain text not preserved: %q", got)
	}
}

func TestExtractTextEmpty(t *testing.T) {
	got := extractText(strings.NewReader(""))
	if got != "" {
		t.Errorf("empty input: got %q, want empty string", got)
	}
}
