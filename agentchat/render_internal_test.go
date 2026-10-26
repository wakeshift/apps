package agentchat

import (
	"strings"
	"testing"
)

// Messages' text is escaped whatever it holds; only the little markdown,
// links and known mentions become markup.
func TestRenderText(t *testing.T) {
	known := func(h string) bool { return h == "ada-codex" || h == "bob" }
	mine := func(h string) bool { return h == "bob" }
	cases := []struct{ in, want string }{
		{`<script>alert(1)</script>`, `<p>&lt;script&gt;alert(1)&lt;/script&gt;</p>`},
		{`<img src=x onerror=alert(1)>`, `<p>&lt;img src=x onerror=alert(1)&gt;</p>`},
		{"a `<b>` c", `<p>a <code>&lt;b&gt;</code> c</p>`},
		{"**bold <i>**", `<p><b>bold &lt;i&gt;</b></p>`},
		{"```go\nif a < b {\n}\n```\nafter", `<pre><span class="lang">go</span><code>if a &lt; b {` + "\n" + `}</code></pre><p>after</p>`},
		{"```\"><script>\nx\n```", "<p>```&#34;&gt;&lt;script&gt;\nx\n```</p>"},
		{"see https://example.com/a?b=1&c=<2>.", `<p>see <a href="https://example.com/a?b=1&amp;c=" rel="noopener noreferrer nofollow ugc" target="_blank">https://example.com/a?b=1&amp;c=</a>&lt;2&gt;.</p>`},
		{`https://x.test/"onmouseover="alert(1)`, `<p><a href="https://x.test/" rel="noopener noreferrer nofollow ugc" target="_blank">https://x.test/</a>&#34;onmouseover=&#34;alert(1)</p>`},
		{"javascript:alert(1) and data:text/html,x", `<p>javascript:alert(1) and data:text/html,x</p>`},
		{"hi @ada-codex and @bob, not @nobody or a@ada-codex", `<p>hi <span class="mention">@ada-codex</span> and <span class="mention me">@bob</span>, not @nobody or a@ada-codex</p>`},
		{"@<b>x", `<p>@&lt;b&gt;x</p>`},
		{"one\n\n\ntwo", `<p>one</p><p>two</p>`},
	}
	for _, c := range cases {
		if got := string(renderText(c.in, known, mine)); got != c.want {
			t.Errorf("renderText(%q)\n got %s\nwant %s", c.in, got, c.want)
		}
	}
	// Whatever goes in, no tag but these comes out.
	out := string(renderText("<a href=x><svg/onload=1><iframe>`<x>`**<y>**```\n<z>\n```", known, mine))
	for _, tag := range []string{"<a href=x", "<svg", "<iframe", "<x>", "<y>", "<z>"} {
		if strings.Contains(out, tag) {
			t.Errorf("%q came through: %s", tag, out)
		}
	}
}
