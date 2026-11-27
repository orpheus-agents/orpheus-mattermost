package config

import (
	"errors"
	"regexp"
	"regexp/syntax"
	"strings"

	"gopkg.in/yaml.v3"
)

// These matchers differ at the right boundary. Markdown can interrupt the
// opening text without ending the line, in which case a separator is required.
type triggerNameMatcher struct {
	line *regexp.Regexp
	span *regexp.Regexp
}

func (w Workflow) HasTriggerNames() bool {
	return w.triggerNames.line != nil
}

func (w Workflow) MatchesTriggerName(text string, lineEnd bool) bool {
	pattern := w.triggerNames.span
	if lineEnd {
		pattern = w.triggerNames.line
	}
	if pattern == nil {
		return false
	}
	match := pattern.FindStringSubmatchIndex(text)
	return match != nil && match[2] < match[3]
}

func parseTriggerNames(names, expression yaml.Node) (triggerNameMatcher, error) {
	var patterns []string
	if !names.IsZero() {
		if names.Kind != yaml.SequenceNode {
			return triggerNameMatcher{}, errors.New("trigger_names must be a list of strings")
		}
		for _, name := range names.Content {
			if name.Tag != "!!str" || name.Value == "" || strings.TrimSpace(name.Value) != name.Value ||
				strings.ContainsAny(name.Value, "\r\n\u0085\u2028\u2029") {
				return triggerNameMatcher{}, errors.New("trigger_names must contain nonempty single-line strings without surrounding whitespace")
			}
			patterns = append(patterns, "(?i:"+regexp.QuoteMeta(name.Value)+")")
		}
	}
	if !expression.IsZero() {
		if expression.Tag != "!!str" {
			return triggerNameMatcher{}, errors.New("trigger_name_regex must be a string")
		}
		if expression.Value != "" {
			parsed, err := syntax.Parse(expression.Value, syntax.Perl|syntax.FoldCase)
			if err != nil {
				return triggerNameMatcher{}, errors.New("trigger_name_regex is not a valid regular expression")
			}
			normalized := parsed.String()
			pattern, err := regexp.Compile(normalized)
			if err != nil {
				return triggerNameMatcher{}, errors.New("trigger_name_regex exceeds regular expression limits")
			}
			if pattern.MatchString("") {
				return triggerNameMatcher{}, errors.New("trigger_name_regex must not match empty text")
			}
			patterns = append(patterns, "(?:"+normalized+")")
		}
	}
	if len(patterns) == 0 {
		return triggerNameMatcher{}, nil
	}
	const separator = `[\t-\r \x{85}\p{Z},:.!?…]`
	prefix := `\A(` + strings.Join(patterns, "|") + `)`
	line, err := regexp.Compile(prefix + `(?:` + separator + `|\z)`)
	if err != nil {
		return triggerNameMatcher{}, errors.New("trigger_names and trigger_name_regex exceed regular expression limits")
	}
	span, err := regexp.Compile(prefix + separator)
	if err != nil {
		return triggerNameMatcher{}, errors.New("trigger_names and trigger_name_regex exceed regular expression limits")
	}
	return triggerNameMatcher{line: line, span: span}, nil
}
