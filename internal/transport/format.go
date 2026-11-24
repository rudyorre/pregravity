package transport

import (
	"html"
	"regexp"
	"strings"
)

var (
	reCodeBlock  = regexp.MustCompile("(?s)```[a-zA-Z0-9_-]*\n?(.*?)```")
	reInlineCode = regexp.MustCompile("`([^`\n]+)`")
	reLink       = regexp.MustCompile(`\[([^\]\n]+)\]\((https?://[^\s)]+)\)`)
	reBoldStar   = regexp.MustCompile(`\*\*([^\n*](?:[^\n]*?[^\n*])?)\*\*`)
	reBoldUnder  = regexp.MustCompile(`(?:^|[^a-zA-Z0-9_])__([^\n_](?:[^\n]*?[^\n_])?)__(?:$|[^a-zA-Z0-9_])`)
	reStrike     = regexp.MustCompile(`~~([^\n~]+)~~`)
	reItalicStar = regexp.MustCompile(`(?:^|[^*\w])\*([^*\n\s](?:[^*\n]*?[^*\n\s])?)\*(?:$|[^*\w])`)
	reHeading    = regexp.MustCompile(`(?m)^#{1,6}\s+(.+)$`)
	reBullet     = regexp.MustCompile(`(?m)^([ \t]*)[*-]\s+`)
)

// MarkdownToTelegramHTML converts common LLM Markdown (bold, italics, inline/fenced code,
// strikethrough, headings, links, bullets) into safe Telegram Bot API HTML.
// Using HTML instead of MarkdownV2 avoids crashes on unescaped dots, dashes, underscores, etc.
func MarkdownToTelegramHTML(md string) string {
	var tokens []string
	stash := func(rendered string) string {
		idx := len(tokens)
		tokens = append(tokens, rendered)
		// Use a control-char sentinel that will never appear in normal text or be touched by regexes.
		return "\x00T" + itoa(idx) + "\x00"
	}

	// 1. Extract fenced code blocks before any other processing.
	s := reCodeBlock.ReplaceAllStringFunc(md, func(m string) string {
		sub := reCodeBlock.FindStringSubmatch(m)
		code := strings.TrimRight(sub[1], "\n")
		return stash("<pre><code>" + html.EscapeString(code) + "</code></pre>")
	})

	// 2. Extract inline code spans.
	s = reInlineCode.ReplaceAllStringFunc(s, func(m string) string {
		sub := reInlineCode.FindStringSubmatch(m)
		return stash("<code>" + html.EscapeString(sub[1]) + "</code>")
	})

	// 3. Extract Markdown links [label](https://...).
	s = reLink.ReplaceAllStringFunc(s, func(m string) string {
		sub := reLink.FindStringSubmatch(m)
		label := html.EscapeString(sub[1])
		href := html.EscapeString(sub[2])
		return stash(`<a href="` + href + `">` + label + `</a>`)
	})

	// 4. Escape all remaining raw HTML characters.
	s = html.EscapeString(s)

	// 5. Convert block-level headings and list bullets.
	s = reHeading.ReplaceAllString(s, "<b>$1</b>")
	s = reBullet.ReplaceAllString(s, "${1}• ")

	// 6. Convert inline emphasis (**bold**, __bold__, ~~strike~~, *italic*).
	s = reBoldStar.ReplaceAllString(s, "<b>$1</b>")
	s = reBoldUnder.ReplaceAllStringFunc(s, func(m string) string {
		idx := strings.Index(m, "__")
		last := strings.LastIndex(m, "__")
		if idx < 0 || last <= idx+2 {
			return m
		}
		return m[:idx] + "<b>" + m[idx+2:last] + "</b>" + m[last+2:]
	})
	s = reStrike.ReplaceAllString(s, "<s>$1</s>")
	s = reItalicStar.ReplaceAllStringFunc(s, func(m string) string {
		first := strings.IndexByte(m, '*')
		last := strings.LastIndexByte(m, '*')
		if first < 0 || last <= first+1 {
			return m
		}
		return m[:first] + "<i>" + m[first+1:last] + "</i>" + m[last+1:]
	})

	// 7. Restore stashed HTML tokens in reverse order (handles any nesting safely).
	for i := len(tokens) - 1; i >= 0; i-- {
		s = strings.ReplaceAll(s, "\x00T"+itoa(i)+"\x00", tokens[i])
	}
	return s
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
