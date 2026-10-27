package notion

import (
	"errors"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Limits of one comment's rich text, as the API enforces them.
const (
	maxRunLen = 2000
	maxRuns   = 100
)

// errCommentTooLong is markdown that needs more runs than one comment holds.
var errCommentTooLong = errors.New("comment: too long for one Notion comment")

// run is one rich text run: its text, its link and its annotations.
type run struct {
	Text                       string
	Link                       string
	Bold, Italic, Strike, Code bool
}

func (r run) sameStyle(o run) bool {
	return r.Link == o.Link && r.Bold == o.Bold && r.Italic == o.Italic && r.Strike == o.Strike && r.Code == o.Code
}

var (
	fence      = regexp.MustCompile("^\\s*(```|~~~)")
	heading    = regexp.MustCompile(`^\s{0,3}#{1,6}\s+`)
	quote      = regexp.MustCompile(`^\s{0,3}>\s?`)
	tableRule  = regexp.MustCompile(`^\s*\|?\s*:?-{3,}:?\s*(\|\s*:?-{3,}:?\s*)*\|?\s*$`)
	blankLines = regexp.MustCompile(`\n{3,}`)
)

// markdownToRichText turns markdown into a comment's rich text. Paragraphs,
// links, bold, italic, strikethrough and inline code survive; headings,
// quotes, lists and tables become their text, and a fenced block code text.
func markdownToRichText(md string) ([]run, error) {
	var (
		runs   []run
		text   strings.Builder
		inCode bool
	)

	flush := func() {
		runs = append(runs, inline(blankLines.ReplaceAllString(text.String(), "\n\n"), run{})...)
		text.Reset()
	}

	for line := range strings.SplitSeq(strings.ReplaceAll(md, "\r\n", "\n"), "\n") {
		if fence.MatchString(line) {
			if !inCode {
				flush()
			}

			inCode = !inCode

			continue
		}

		if inCode {
			runs = append(runs, run{Text: line + "\n", Code: true})

			continue
		}

		switch {
		case tableRule.MatchString(line) && strings.Contains(line, "|"):
			continue
		case strings.HasPrefix(strings.TrimSpace(line), "|"):
			cells := strings.Split(strings.Trim(strings.TrimSpace(line), "|"), "|")
			for i := range cells {
				cells[i] = strings.TrimSpace(cells[i])
			}

			line = strings.Join(cells, " | ")
		default:
			line = quote.ReplaceAllString(heading.ReplaceAllString(line, ""), "")
		}

		text.WriteString(line + "\n")
	}

	flush()

	runs = trimRuns(merge(runs))

	var out []run

	for _, r := range runs {
		out = append(out, split(r)...)
	}

	if len(out) > maxRuns {
		return nil, errCommentTooLong
	}

	return out, nil
}

// commentRichText converts markdown to a comment's rich text in the
// request shape both transports send.
func commentRichText(markdown string) ([]any, error) {
	runs, err := markdownToRichText(markdown)
	if err != nil {
		return nil, err
	}

	rich := make([]any, len(runs))
	for i, r := range runs {
		text := map[string]any{"content": r.Text}
		if r.Link != "" {
			text["link"] = map[string]any{"url": r.Link}
		}

		rich[i] = map[string]any{
			"type": "text",
			"text": text,
			"annotations": map[string]any{
				"bold": r.Bold, "italic": r.Italic, "strikethrough": r.Strike, "code": r.Code,
			},
		}
	}

	return rich, nil
}

// inline parses the inline forms in s, each run styled on top of style.
func inline(s string, style run) []run {
	var (
		runs []run
		lit  strings.Builder
	)

	emit := func(rs ...run) {
		if lit.Len() > 0 {
			r := style
			r.Text = lit.String()
			runs = append(runs, r)
			lit.Reset()
		}

		runs = append(runs, rs...)
	}

	for i := 0; i < len(s); {
		rest := s[i:]

		switch {
		case rest[0] == '\\' && len(rest) > 1 && strings.ContainsRune("\\`*_~[]()#", rune(rest[1])):
			lit.WriteByte(rest[1])
			i += 2

			continue
		case rest[0] == '`':
			if end := strings.IndexByte(rest[1:], '`'); end > 0 {
				r := style
				r.Text, r.Code = rest[1:1+end], true
				emit(r)
				i += end + 2

				continue
			}
		case rest[0] == '[':
			if text, link, n, ok := linkAt(rest); ok {
				inner := style
				inner.Link = link
				emit(inline(text, inner)...)
				i += n

				continue
			}
		case strings.HasPrefix(rest, "**") || strings.HasPrefix(rest, "__"):
			if end := closing(rest[2:], rest[:2]); end > 0 {
				inner := style
				inner.Bold = true
				emit(inline(rest[2:2+end], inner)...)
				i += end + 4

				continue
			}
		case strings.HasPrefix(rest, "~~"):
			if end := closing(rest[2:], "~~"); end > 0 {
				inner := style
				inner.Strike = true
				emit(inline(rest[2:2+end], inner)...)
				i += end + 4

				continue
			}
		case rest[0] == '*' || (rest[0] == '_' && wordBoundaryBefore(s, i)):
			if end := closingSingle(rest[1:], rest[0]); end > 0 {
				inner := style
				inner.Italic = true
				emit(inline(rest[1:1+end], inner)...)
				i += end + 2

				continue
			}
		}

		_, size := utf8.DecodeRuneInString(rest)
		lit.WriteString(rest[:size])
		i += size
	}

	emit()

	return runs
}

// linkAt reads [text](url) at the start of s: the text, the url and the
// bytes it spans.
func linkAt(s string) (string, string, int, bool) {
	mid := strings.Index(s, "](")
	if mid < 0 || strings.Contains(s[1:mid], "\n") {
		return "", "", 0, false
	}

	end := strings.IndexByte(s[mid+2:], ')')
	if end < 0 {
		return "", "", 0, false
	}

	link := s[mid+2 : mid+2+end]
	if link == "" || strings.ContainsAny(link, " \n") {
		return "", "", 0, false
	}

	return s[1:mid], link, mid + 3 + end, true
}

// closing finds the closing marker for a non-empty span, or -1.
func closing(s, marker string) int {
	end := strings.Index(s, marker)
	if end <= 0 || strings.TrimSpace(s[:end]) == "" {
		return -1
	}

	return end
}

// closingSingle finds a closing * or _ that is not half of a double marker;
// a closing _ must end a word.
func closingSingle(s string, marker byte) int {
	for i := 1; i < len(s); i++ {
		if s[i] != marker {
			continue
		}

		if i+1 < len(s) && s[i+1] == marker {
			i++

			continue
		}

		if marker == '_' && i+1 < len(s) && isWordByte(s[i+1]) {
			continue
		}

		if s[0] == ' ' || s[i-1] == ' ' {
			return -1
		}

		return i
	}

	return -1
}

func wordBoundaryBefore(s string, i int) bool { return i == 0 || !isWordByte(s[i-1]) }

func isWordByte(b byte) bool {
	return b == '_' || unicode.IsLetter(rune(b)) || unicode.IsDigit(rune(b))
}

// merge joins neighbouring runs of the same style.
func merge(runs []run) []run {
	var out []run

	for _, r := range runs {
		if r.Text == "" {
			continue
		}

		if n := len(out); n > 0 && out[n-1].sameStyle(r) {
			out[n-1].Text += r.Text

			continue
		}

		out = append(out, r)
	}

	return out
}

// trimRuns drops the whitespace around the whole text.
func trimRuns(runs []run) []run {
	for len(runs) > 0 {
		runs[0].Text = strings.TrimLeft(runs[0].Text, " \t\n")
		if runs[0].Text != "" {
			break
		}

		runs = runs[1:]
	}

	for len(runs) > 0 {
		last := &runs[len(runs)-1]

		last.Text = strings.TrimRight(last.Text, " \t\n")
		if last.Text != "" {
			break
		}

		runs = runs[:len(runs)-1]
	}

	return runs
}

// split cuts a run into runs of at most maxRunLen characters.
func split(r run) []run {
	var out []run

	for utf8.RuneCountInString(r.Text) > maxRunLen {
		cut := 0
		for range maxRunLen {
			_, size := utf8.DecodeRuneInString(r.Text[cut:])
			cut += size
		}

		head := r
		head.Text = r.Text[:cut]
		out = append(out, head)
		r.Text = r.Text[cut:]
	}

	return append(out, r)
}
