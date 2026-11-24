package transport

import (
	"strings"
	"testing"
)

func TestMarkdownToTelegramHTML(t *testing.T) {
	in := strings.Join([]string{
		"### Morning Preflight",
		"- **High priority:** fix `snake_case_var` & *double check* <tags>",
		"* Also check [Antigravity](https://antigravity.google.com) for ~~old~~ stuff",
		"```go",
		"if a < b && c > 0 { fmt.Println(\"**not bold**\") }",
		"```",
	}, "\n")

	got := MarkdownToTelegramHTML(in)

	wantContains := []string{
		"<b>Morning Preflight</b>",
		"• <b>High priority:</b> fix <code>snake_case_var</code> &amp; <i>double check</i> &lt;tags&gt;",
		`• Also check <a href="https://antigravity.google.com">Antigravity</a> for <s>old</s> stuff`,
		`<pre><code>if a &lt; b &amp;&amp; c &gt; 0 { fmt.Println(&#34;**not bold**&#34;) }</code></pre>`,
	}
	for _, want := range wantContains {
		if !strings.Contains(got, want) {
			t.Errorf("expected HTML to contain:\n  %s\ngot:\n%s", want, got)
		}
	}
}

func TestMarkdownToTelegramHTMLPreservesSnakeCase(t *testing.T) {
	got := MarkdownToTelegramHTML("Check list_tasks and create_handoff for t-681a62.")
	if got != "Check list_tasks and create_handoff for t-681a62." {
		t.Fatalf("unexpected mutation of snake_case: %q", got)
	}
}
