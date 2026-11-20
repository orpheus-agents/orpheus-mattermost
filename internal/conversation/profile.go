package conversation

import (
	"cmp"
	"errors"
	"regexp"
	"strings"

	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/text"
)

var ErrConflictingProfileTags = errors.New("tags select different model profiles or sandbox templates")

// Keep Markdown delimiters inside a bare URL in its link node (e.g. /path/**#tag**).
var urlTail = regexp.MustCompile(`^[^\s\p{Z}<>\[\]"']+`)
var profileMarkdown = goldmark.New(goldmark.WithExtensions(extension.NewLinkify(
	extension.WithLinkifyURLRegexp(urlTail), extension.WithLinkifyWWWRegexp(urlTail),
)))
var profileToken = regexp.MustCompile(`^#([A-Za-z][A-Za-z0-9_-]*)[.,;:!?…"'\p{Pe}\p{Pf}]*$`)

// taggedProfile reads only literal prose in the post that first triggers the thread.
func taggedProfile(body string, tags map[string]config.TagTarget, defaultTemplate string) (config.TagTarget, error) {
	if len(tags) == 0 || !strings.Contains(body, "#") {
		return config.TagTarget{}, nil
	}
	source := []byte(body)
	tree := profileMarkdown.Parser().Parse(text.NewReader(source))
	var target config.TagTarget
	err := ast.Walk(tree, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch n.Kind() {
		case ast.KindBlockquote, ast.KindFencedCodeBlock, ast.KindCodeBlock, ast.KindCodeSpan, ast.KindLink, ast.KindImage, ast.KindAutoLink, ast.KindRawHTML, ast.KindHTMLBlock:
			return ast.WalkSkipChildren, nil
		}
		node, ok := n.(*ast.Text)
		if !ok {
			return ast.WalkContinue, nil
		}
		for word := range strings.FieldsSeq(string(node.Segment.Value(source))) {
			match := profileToken.FindStringSubmatch(word)
			if match == nil {
				continue
			}
			selected, ok := tags[strings.ToLower(match[1])]
			if !ok {
				continue
			}
			if target.Profile != "" && (selected.Profile != target.Profile || cmp.Or(selected.SandboxTemplate, defaultTemplate) != cmp.Or(target.SandboxTemplate, defaultTemplate)) {
				return ast.WalkStop, ErrConflictingProfileTags
			}
			// An explicit template stays pinned even when another alias inherits it.
			target.Profile = selected.Profile
			target.SandboxTemplate = cmp.Or(selected.SandboxTemplate, target.SandboxTemplate)
		}
		return ast.WalkContinue, nil
	})
	if err != nil {
		return config.TagTarget{}, err
	}
	return target, nil
}
