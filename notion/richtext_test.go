package notion

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestMarkdownToRichText(t *testing.T) {
	for _, tc := range []struct {
		name, md string
		want     []run
	}{
		{"plain", "Just text.", []run{{Text: "Just text."}}},
		{"bold and italic", "a **b** *c* _d_ e", []run{
			{Text: "a "},
			{Text: "b", Bold: true},
			{Text: " "},
			{Text: "c", Italic: true},
			{Text: " "},
			{Text: "d", Italic: true},
			{Text: " e"},
		}},
		{"code and strike", "run `just test`, ~~not make~~", []run{
			{Text: "run "}, {Text: "just test", Code: true}, {Text: ", "}, {Text: "not make", Strike: true},
		}},
		{"bold link", "see [the **report**](https://x.test/r).", []run{
			{Text: "see "},
			{Text: "the ", Link: "https://x.test/r"},
			{Text: "report", Link: "https://x.test/r", Bold: true},
			{Text: "."},
		}},
		{"unclosed and snake_case", "a **b and snake_case_name * c", []run{{Text: "a **b and snake_case_name * c"}}},
		{"escape", `not \*italic\*`, []run{{Text: "not *italic*"}}},
		{"paragraphs and blocks", "# Result\n\n> quoted\n\n- one\n- two\n\n\n\n| a | b |\n|---|---|\n| 1 | 2 |", []run{
			{Text: "Result\n\nquoted\n\n- one\n- two\n\na | b\n1 | 2"},
		}},
		{"fence", "before\n\n```go\nx := 1\n```\nafter", []run{
			{Text: "before\n\n"}, {Text: "x := 1\n", Code: true}, {Text: "after"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := markdownToRichText(tc.md)
			if err != nil || !slices.Equal(got, tc.want) {
				t.Fatalf("got %+v, err %v\nwant %+v", got, err, tc.want)
			}
		})
	}
}

func TestMarkdownToRichTextLimits(t *testing.T) {
	got, err := markdownToRichText(strings.Repeat("é", 4500))
	if err != nil || len(got) != 3 || len([]rune(got[0].Text)) != maxRunLen || len([]rune(got[2].Text)) != 500 {
		t.Fatalf("want three runs of 2000, 2000 and 500, got %d runs, err %v", len(got), err)
	}

	if _, err := markdownToRichText(strings.Repeat("a **b** ", 60)); !errors.Is(err, errCommentTooLong) {
		t.Fatalf("err = %v, want errCommentTooLong", err)
	}
}
