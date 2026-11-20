package conversation

import (
	"errors"
	"regexp"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/text"
)

var ErrConflictingProfileTags = errors.New("tags select different model profiles")

// Keep Markdown delimiters inside a bare URL in its link node (e.g. /path/**#tag**).
var urlTail = regexp.MustCompile(`^[^\s\p{Z}<>\[\]"']+`)
var profileMarkdown = goldmark.New(goldmark.WithExtensions(extension.NewLinkify(
	extension.WithLinkifyURLRegexp(urlTail), extension.WithLinkifyWWWRegexp(urlTail),
)))
var profileToken = regexp.MustCompile(`^#([A-Za-z][A-Za-z0-9_-]*)[.,;:!?…"'\p{Pe}\p{Pf}]*$`)

// taggedProfile reads only literal prose in the post that first triggers the thread.
func taggedProfile(body string, tags map[string]string) (string, error) {
	if len(tags) == 0 || !strings.Contains(body, "#") {
		return "", nil
	}
	source := []byte(body)
	tree := profileMarkdown.Parser().Parse(text.NewReader(source))
	var profile string
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
			if profile != "" && selected != profile {
				return ast.WalkStop, ErrConflictingProfileTags
			}
			profile = selected
		}
		return ast.WalkContinue, nil
	})
	if err != nil {
		return "", err
	}
	return profile, nil
}
