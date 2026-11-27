package conversation

import (
	"strings"
	"unicode"

	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

// AddressesBot tests the workflow's address forms for human and allowed
// external bot posts. Self-triggering requires an explicit username mention.
func AddressesBot(body string, workflow config.Workflow, username string) bool {
	if strings.Contains(body, "@") && Mention(body, []string{username}) {
		return true
	}
	if !workflow.HasTriggerNames() {
		return false
	}

	source := []byte(body)
	doc := proseMarkdown.Parser().Parse(text.NewReader(source))
	paragraph, ok := doc.FirstChild().(*ast.Paragraph)
	if !ok {
		return false
	}
	line := paragraph.Lines().At(0)
	if strings.TrimSpace(body[:line.Start]) != "" {
		return false
	}
	lineEnd := line.Start + len(strings.TrimRight(body[line.Start:line.Stop], "\r\n"))

	// Follow contiguous text in the opening line. Inline Markdown and links
	// end the span, while text on later lines cannot extend the address.
	end := line.Start
	for child := paragraph.FirstChild(); child != nil && end < lineEnd; child = child.NextSibling() {
		node, ok := child.(*ast.Text)
		if !ok || node.Segment.Start != end {
			break
		}
		end = min(node.Segment.Stop, lineEnd)
	}
	// Keep trailing source whitespace that Goldmark omits so explicit regex anchors see the full line.
	if strings.TrimSpace(body[end:lineEnd]) == "" {
		end = lineEnd
	}
	return workflow.MatchesTriggerName(strings.TrimLeftFunc(body[line.Start:end], unicode.IsSpace), end == lineEnd)
}
