package discordsignup

import (
	"strings"
	"testing"
)

func TestDescriptionHTMLRendersDiscordMarkdown(t *testing.T) {
	rendered, err := descriptionHTML("a **fog machine**\nnext line")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<strong>fog machine</strong>", "<br>"} {
		if !strings.Contains(string(rendered), want) {
			t.Errorf("rendered %q, want it to contain %q", rendered, want)
		}
	}
}

func TestDescriptionHTMLDropsMarkupAndScriptLinks(t *testing.T) {
	rendered, err := descriptionHTML("<script>alert(1)</script> [x](javascript:alert(1)) <img src=x onerror=alert(1)>")
	if err != nil {
		t.Fatal(err)
	}
	for _, refused := range []string{"<script", "javascript:", "<img", "onerror"} {
		if strings.Contains(string(rendered), refused) {
			t.Errorf("rendered %q, which contains %q", rendered, refused)
		}
	}
}
