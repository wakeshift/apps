package agentchat

import (
	"html"
	"html/template"
	"regexp"
	"strings"
)

// Messages' text on the site: everything escaped, with a little markdown
// (fenced code blocks, `inline code`, **bold**), links (http and https
// only, opening apart from the site: rel="noopener noreferrer nofollow
// ugc"), and @-mentions of the channel's handles highlighted ("me" for the
// person looking's own). Nothing else in a message becomes markup.

var (
	// fenceRE is a fenced code block: ``` and an optional language, its
	// lines, and ``` on a line of its own (or the end of the text).
	fenceRE   = regexp.MustCompile("(?s)```([A-Za-z0-9_+-]{0,20})[ \t]*\n(.*?)(?:\n```[ \t]*(?:\n|$)|$)")
	inlineRE  = regexp.MustCompile("`([^`\n]+)`|\\*\\*([^*\n]+)\\*\\*|https?://[^\\s<>\"'`]+|(?:^|[^A-Za-z0-9._%+-])@([A-Za-z0-9][A-Za-z0-9-]*)")
	trailerRE = regexp.MustCompile(`[.,;:!?)\]}'"]+$`)
	paraRE    = regexp.MustCompile(`\n[ \t]*\n\s*`)
)

// renderText is a message's text as HTML. known says whether an @handle is
// one to highlight, and mine whether it's the person looking's.
func renderText(text string, known, mine func(handle string) bool) template.HTML {
	var b strings.Builder
	rest := text
	for {
		loc := fenceRE.FindStringSubmatchIndex(rest)
		if loc == nil {
			paragraphs(&b, rest, known, mine)
			break
		}
		paragraphs(&b, rest[:loc[0]], known, mine)
		lang, code := rest[loc[2]:loc[3]], rest[loc[4]:loc[5]]
		b.WriteString("<pre>")
		if lang != "" {
			b.WriteString(`<span class="lang">` + html.EscapeString(lang) + `</span>`)
		}
		b.WriteString("<code>" + html.EscapeString(code) + "</code></pre>")
		rest = rest[loc[1]:]
	}
	return template.HTML(b.String())
}

// paragraphs writes text's paragraphs (split at blank lines; single line
// breaks kept by the page's styles), each with its inline markup.
func paragraphs(b *strings.Builder, text string, known, mine func(string) bool) {
	for _, p := range paraRE.Split(strings.Trim(text, "\n"), -1) {
		if strings.TrimSpace(p) == "" {
			continue
		}
		b.WriteString("<p>")
		inline(b, p, known, mine)
		b.WriteString("</p>")
	}
}

func inline(b *strings.Builder, s string, known, mine func(string) bool) {
	last := 0
	for _, m := range inlineRE.FindAllStringSubmatchIndex(s, -1) {
		start, end := m[0], m[1]
		switch {
		case m[2] >= 0: // `code`
			b.WriteString(html.EscapeString(s[last:start]))
			b.WriteString("<code>" + html.EscapeString(s[m[2]:m[3]]) + "</code>")
		case m[4] >= 0: // **bold**
			b.WriteString(html.EscapeString(s[last:start]))
			b.WriteString("<b>" + html.EscapeString(s[m[4]:m[5]]) + "</b>")
		case m[6] >= 0: // @handle (after the character before it, if any)
			at := m[6] - 1
			handle := strings.ToLower(s[m[6]:m[7]])
			if !known(handle) {
				continue
			}
			b.WriteString(html.EscapeString(s[last:at]))
			class := "mention"
			if mine(handle) {
				class += " me"
			}
			b.WriteString(`<span class="` + class + `">` + html.EscapeString(s[at:end]) + `</span>`)
		default: // a link, without the punctuation after it
			link := s[start:end]
			trail := trailerRE.FindString(link)
			link = link[:len(link)-len(trail)]
			end -= len(trail)
			b.WriteString(html.EscapeString(s[last:start]))
			b.WriteString(`<a href="` + html.EscapeString(link) + `" rel="noopener noreferrer nofollow ugc" target="_blank">` + html.EscapeString(link) + `</a>`)
		}
		last = end
	}
	b.WriteString(html.EscapeString(s[last:]))
}
