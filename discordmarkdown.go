package discordsignup

import (
	"bytes"
	"fmt"
	"html/template"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/renderer/html"
)

// descriptionMarkdown renders an event description as the web pages show it.
// People write descriptions for Discord, which reads them as markdown, so a
// page that printed the raw text showed **bold** with its asterisks.
//
// goldmark without WithUnsafe drops raw HTML and refuses javascript: links,
// so a description cannot put markup of its own on the page. Hard wraps keep
// each line on its own line, as Discord shows it. Discord's __underline__
// comes out bold and its ||spoilers|| as typed; neither is CommonMark.
var descriptionMarkdown = goldmark.New(
	goldmark.WithExtensions(extension.Strikethrough, extension.Linkify),
	goldmark.WithRendererOptions(html.WithHardWraps()),
)

func descriptionHTML(description string) (template.HTML, error) {
	var rendered bytes.Buffer
	if err := descriptionMarkdown.Convert([]byte(description), &rendered); err != nil {
		return "", fmt.Errorf("render description markdown: %w", err)
	}
	return template.HTML(rendered.String()), nil
}
