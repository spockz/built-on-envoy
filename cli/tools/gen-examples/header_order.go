// Copyright Built On Envoy
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

// Comparison keeps HTTP field order independent without changing recorded curl output or byte metadata.
package main

import (
	"encoding/base64"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

func canonicalHeaderOrder(block outputBlock, captures []capture) (outputBlock, []capture) {
	lines := transcriptLines(block.body)
	order := make([]int, len(lines))
	for index := range order {
		order[index] = index
	}
	sections := responseHeaderSections(block)
	for _, section := range sections {
		ordered := append([]responseHeader(nil), section.headers...)
		sort.SliceStable(ordered, func(left, right int) bool { return ordered[left].name < ordered[right].name })
		for index, header := range ordered {
			order[section.headers[index].line] = header.line
		}
	}
	var body strings.Builder
	offsets := make([]int, len(lines))
	changed := false
	for index, original := range order {
		changed = changed || index != original
		offsets[original] = body.Len()
		body.WriteString(lines[original].text)
	}
	if !changed {
		return block, captures
	}
	remapped, valid := remapHeaderCaptures(captures, sections, lines, offsets)
	if !valid {
		return block, captures
	}
	metadata, valid := reorderOutputMetadata(block, order, body.String())
	if !valid {
		return block, captures
	}
	block.body, block.metadata = body.String(), metadata
	return block, remapped
}

func remapHeaderCaptures(captures []capture, sections []responseHeaderSection, lines []transcriptLine, offsets []int) ([]capture, bool) {
	remapped := append([]capture(nil), captures...)
	for index, value := range remapped {
		for _, section := range sections {
			for _, header := range section.headers {
				line := lines[header.line]
				end := line.offset + len(line.text)
				if value.end <= line.offset || value.start >= end {
					continue
				}
				// Cross-line comparison captures can encode order-sensitive context.
				if value.start < line.offset || value.end > end {
					return nil, false
				}
				delta := offsets[header.line] - line.offset
				remapped[index].start += delta
				remapped[index].end += delta
			}
		}
	}
	return remapped, true
}

type capturedOutputMetadata struct {
	stream            string
	endings           []string
	bareCR            []int
	whitespace        []lineWhitespace
	noTrailingNewline bool
}

func isOutputMetadata(line string) bool {
	return (strings.HasPrefix(line, "<!-- stdout") || strings.HasPrefix(line, "<!-- stderr")) && strings.HasSuffix(line, " -->\n")
}

func parseOutputMetadata(line string) (capturedOutputMetadata, bool) {
	var metadata capturedOutputMetadata
	if !isOutputMetadata(line) {
		return metadata, false
	}
	fields := strings.Split(strings.TrimSuffix(strings.TrimPrefix(line, "<!-- "), " -->\n"), "; ")
	metadata.stream = fields[0]
	if metadata.stream != "stdout" && metadata.stream != "stderr" {
		return metadata, false
	}
	for _, field := range fields[1:] {
		switch {
		case strings.HasPrefix(field, "Line endings: "):
			metadata.endings = strings.Split(strings.TrimPrefix(field, "Line endings: "), ",")
		case strings.HasPrefix(field, "Bare CR offsets: "):
			for _, number := range strings.Split(strings.TrimPrefix(field, "Bare CR offsets: "), ",") {
				offset, err := strconv.Atoi(number)
				if err != nil {
					return metadata, false
				}
				metadata.bareCR = append(metadata.bareCR, offset)
			}
		case strings.HasPrefix(field, "Trailing whitespace: "):
			encoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(field, "Trailing whitespace: "))
			if err != nil {
				return metadata, false
			}
			if err := json.Unmarshal(encoded, &metadata.whitespace); err != nil {
				return metadata, false
			}
		case field == "No trailing newline":
			metadata.noTrailingNewline = true
		default:
			return metadata, false
		}
	}
	return metadata, true
}

func restoreCapturedOutput(body string, metadata *capturedOutputMetadata) (string, bool) {
	lines := transcriptLines(body)
	for _, whitespace := range metadata.whitespace {
		if whitespace.Line < 0 || whitespace.Line >= len(lines) {
			return "", false
		}
		line := &lines[whitespace.Line]
		line.text = strings.TrimSuffix(line.text, "\n") + whitespace.Text + "\n"
	}
	if metadata.noTrailingNewline && len(lines) > 0 {
		last := &lines[len(lines)-1]
		last.text = strings.TrimSuffix(last.text, "\n")
	}
	var restored strings.Builder
	endingIndex := 0
	for _, line := range lines {
		text := line.text
		if strings.HasSuffix(text, "\n") {
			if endingIndex >= len(metadata.endings) {
				return "", false
			}
			switch metadata.endings[endingIndex] {
			case "CRLF":
				text = strings.TrimSuffix(text, "\n") + "\r\n"
			case "LF":
			default:
				return "", false
			}
			endingIndex++
		}
		restored.WriteString(text)
	}
	if endingIndex != len(metadata.endings) {
		return "", false
	}
	return restoreBareCR(restored.String(), metadata.bareCR)
}

func restoreBareCR(escaped string, offsets []int) (string, bool) {
	var restored strings.Builder
	position := 0
	for index, offset := range offsets {
		if offset < 0 || offset > len(escaped) || index > len(escaped)-offset {
			return "", false
		}
		start := offset + index
		if start < position || start+2 > len(escaped) || escaped[start:start+2] != `\r` {
			return "", false
		}
		restored.WriteString(escaped[position:start])
		restored.WriteByte('\r')
		position = start + 2
	}
	restored.WriteString(escaped[position:])
	return restored.String(), true
}

func reorderOutputMetadata(block outputBlock, order []int, body string) (string, bool) {
	if block.metadata == "" {
		return "", true
	}
	metadata, valid := parseOutputMetadata(block.metadata)
	if !valid {
		return "", false
	}
	raw, valid := restoreCapturedOutput(block.body, &metadata)
	if !valid {
		return "", false
	}
	rendered, err := renderOutput(raw, nil, nil, metadata.stream)
	// A round trip rejects malformed metadata and preserves its exact encoding conventions.
	if err != nil || rendered != renderFencedBlock("text", block.body)+"\n"+block.metadata {
		return "", false
	}
	lines := transcriptLines(raw)
	if len(lines) != len(order) {
		return "", false
	}
	for _, section := range responseHeaderSections(block) {
		for _, header := range section.headers {
			if strings.ContainsRune(strings.TrimSuffix(lines[header.line].text, "\r\n"), '\r') {
				return "", false
			}
		}
	}
	var reordered strings.Builder
	for _, original := range order {
		reordered.WriteString(lines[original].text)
	}
	rendered, err = renderOutput(reordered.String(), nil, nil, metadata.stream)
	if err != nil {
		return "", false
	}
	for _, part := range splitTranscript(rendered) {
		if part.block != nil {
			return part.block.metadata, part.block.body == body
		}
	}
	return "", false
}
