// Copyright Built On Envoy
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

// Output elision belongs beside comparison so omitted lines cannot weaken execution or retained-value checks.
package main

import (
	"strings"

	"github.com/tetratelabs/built-on-envoy/cli/internal/extensions"
)

func reconcileElidedTranscript(old, current string, rules []extensions.ExampleComparisonRule, headers []string) (string, error) {
	compiled, err := compileComparisonRules(rules)
	if err != nil {
		return "", err
	}
	headerRules, err := compileHeaderRules(headers)
	if err != nil {
		return "", err
	}
	oldParts, newParts := splitTranscript(old), splitTranscript(current)
	if len(oldParts) != len(newParts) {
		return current, nil
	}
	var projected strings.Builder
	cursor := 0
	equal := true
	for index, oldPart := range oldParts {
		newPart := newParts[index]
		if oldPart.block == nil || newPart.block == nil {
			if oldPart.block != nil || newPart.block != nil || oldPart.literal != newPart.literal {
				return current, nil
			}
			continue
		}
		previous, fresh := *oldPart.block, *newPart.block
		if previous.kind != fresh.kind || previous.raw != fresh.raw {
			return current, nil
		}
		if !previous.raw || !hasElision(previous.body) {
			equal = equivalentOutputBlock(previous, fresh, compiled, headerRules) && equal
			continue
		}
		replacement, matched := projectElidedOutput(previous, fresh, compiled, headerRules)
		if !matched {
			equal = false
			continue
		}
		projected.WriteString(current[cursor:fresh.start])
		projected.WriteString(replacement)
		cursor = fresh.end
		for _, part := range splitTranscript(replacement) {
			if part.block != nil {
				equal = previous.metadata == part.block.metadata && equal
			}
		}
	}
	if equal {
		return old, nil
	}
	projected.WriteString(current[cursor:])
	return projected.String(), nil
}

func hasElision(body string) bool {
	for _, line := range transcriptLines(body) {
		if isElisionLine(line.text) {
			return true
		}
	}
	return false
}

func isElisionLine(line string) bool { return line == "...\n" || line == "..." }

func projectElidedOutput(old, current outputBlock, rules []comparisonRule, headers map[string]struct{}) (string, bool) {
	metadata, valid := parseOutputMetadata(current.metadata)
	if !valid {
		return "", false
	}
	if old.metadata != "" {
		previous, parsed := parseOutputMetadata(old.metadata)
		if !parsed || previous.stream != metadata.stream {
			return "", false
		}
	}
	raw, valid := restoreCapturedOutput(current.body, &metadata)
	if !valid {
		return "", false
	}
	rendered, err := renderOutput(raw, nil, nil, metadata.stream)
	if err != nil || rendered != renderFencedBlock("text", current.body)+"\n"+current.metadata {
		return "", false
	}
	matcher := newElisionMatcher(old.body, current, rules, headers)
	if !matcher.match(0, 0) {
		return "", false
	}
	selected := make(map[int]bool, len(matcher.selected))
	for _, index := range matcher.selected {
		selected[index] = true
	}
	for _, index := range matcher.statuses {
		if !selected[index] {
			return "", false
		}
	}
	rawLines := transcriptLines(raw)
	if len(rawLines) != len(matcher.actual) {
		return "", false
	}
	var output strings.Builder
	for _, index := range matcher.selected {
		if index < 0 {
			output.WriteString("...\n")
		} else {
			output.WriteString(rawLines[index].text)
		}
	}
	rendered, err = renderOutput(output.String(), nil, nil, metadata.stream)
	return rendered, err == nil
}

type elisionPosition struct{ expected, actual int }

type elisionMatcher struct {
	expected []transcriptLine
	actual   []transcriptLine
	selected []int
	statuses []int
	failed   map[elisionPosition]bool
	sections map[int]responseHeaderSection
	volatile map[int]bool
	block    outputBlock
	rules    []comparisonRule
	headers  map[string]struct{}
}

func newElisionMatcher(pattern string, block outputBlock, rules []comparisonRule, headers map[string]struct{}) *elisionMatcher {
	m := &elisionMatcher{
		expected: transcriptLines(pattern), actual: transcriptLines(block.body),
		failed: make(map[elisionPosition]bool), sections: make(map[int]responseHeaderSection),
		volatile: make(map[int]bool), block: block, rules: rules, headers: headers,
	}
	m.selected = make([]int, len(m.expected))
	if index, _, found := responseHeaderStart(block, m.actual); found {
		m.statuses = append(m.statuses, index)
	}
	for _, section := range responseHeaderSections(block) {
		m.statuses = append(m.statuses, section.statusLine)
		if len(section.headers) > 0 {
			m.sections[section.headers[0].line] = section
		}
	}
	for _, value := range outputCaptures(block, rules, headers) {
		for index, line := range m.actual {
			if value.start >= line.offset && value.end <= line.offset+len(line.text) {
				m.volatile[index] = true
				break
			}
		}
	}
	return m
}

func (m *elisionMatcher) match(expected, actual int) bool {
	position := elisionPosition{expected: expected, actual: actual}
	if m.failed[position] {
		return false
	}
	if expected == len(m.expected) {
		return actual == len(m.actual)
	}
	if section, found := m.sections[actual]; found {
		if next, handled, matched := m.matchHeaders(expected, section); handled {
			if matched && m.match(next, section.headers[len(section.headers)-1].line+1) {
				return true
			}
			m.failed[position] = true
			return false
		}
	}
	if isElisionLine(m.expected[expected].text) {
		m.selected[expected] = -1
		for end := actual + 1; end <= len(m.actual); end++ {
			if m.match(expected+1, end) {
				return true
			}
		}
	} else if actual < len(m.actual) && m.lineMatches(expected, actual) {
		m.selected[expected] = actual
		if m.match(expected+1, actual+1) {
			return true
		}
	}
	m.failed[position] = true
	return false
}

func (m *elisionMatcher) lineMatches(expected, actual int) bool {
	pattern, line := m.expected[expected].text, m.actual[actual]
	if pattern == line.text {
		return true
	}
	if !m.volatile[actual] {
		return false
	}
	// Borrow the full captured context so omitted framing cannot turn body text into HTTP headers.
	previous, current := m.block, m.block
	previous.metadata, current.metadata = "", ""
	previous.body = current.body[:line.offset] + pattern + current.body[line.offset+len(line.text):]
	return equivalentOutputBlock(previous, current, m.rules, m.headers)
}

func (m *elisionMatcher) matchHeaders(start int, section responseHeaderSection) (next int, handled, matched bool) {
	prefix := ""
	if strings.HasPrefix(m.actual[section.headers[0].line].text, "< ") {
		prefix = "< "
	}
	separator := section.headers[len(section.headers)-1].line + 1
	queues := make(map[string][]int)
	for _, header := range section.headers {
		queues[header.name] = append(queues[header.name], header.line)
	}
	var names []string
	elisions := 0
	for next = start; next < len(m.expected); next++ {
		line := m.expected[next].text
		if line == m.actual[separator].text {
			break
		}
		if isElisionLine(line) {
			elisions++
			names = append(names, "")
			continue
		}
		payload, valid := responseLine(m.expected[next], prefix)
		if !valid {
			return 0, false, false
		}
		match := rawHeaderPattern.FindStringSubmatchIndex(payload)
		if match == nil || !validHeaderName(strings.ToLower(payload[match[2]:match[3]])) {
			return 0, false, false
		}
		names = append(names, strings.ToLower(payload[match[2]:match[3]]))
	}
	if next == len(m.expected) {
		return 0, false, false
	}
	retained := len(names) - elisions
	omitted := len(section.headers) - retained
	if elisions == 0 && omitted != 0 || elisions > 0 && omitted < elisions {
		return next, true, false
	}
	for offset, name := range names {
		index := start + offset
		m.selected[index] = -1
		if name == "" {
			continue
		}
		found := false
		for len(queues[name]) > 0 {
			candidate := queues[name][0]
			queues[name] = queues[name][1:]
			if m.lineMatches(index, candidate) {
				m.selected[index], found = candidate, true
				break
			}
			if elisions == 0 {
				return next, true, false
			}
		}
		if !found {
			return next, true, false
		}
	}
	return next, true, true
}
