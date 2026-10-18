// Copyright Built On Envoy
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

// Rendered transcript tests prove that elision changes verification scope without inventing retained output.
package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tetratelabs/built-on-envoy/cli/internal/extensions"
)

func TestElidedTranscriptWholeLines(t *testing.T) {
	for _, tc := range []struct {
		name, pattern, actual string
		equal                 bool
	}{
		{name: "middle", pattern: "first\n...\nlast\n", actual: "first\nchanging\nmore\nlast\n", equal: true},
		{name: "leading and trailing", pattern: "...\nanchor\n...\n", actual: "first\nanchor\nlast\n", equal: true},
		{name: "consecutive", pattern: "...\n...\nlast\n", actual: "one\ntwo\nlast\n", equal: true},
		{name: "whole stream", pattern: "...\n", actual: "one\ntwo\n", equal: true},
		{name: "blank line", pattern: "first\n...\nlast\n", actual: "first\n\nlast\n", equal: true},
		{name: "backtracking", pattern: "...\nanchor\nlast\n", actual: "one\nanchor\nwrong\nanchor\nlast\n", equal: true},
		{name: "requires at least one line", pattern: "first\n...\nlast\n", actual: "first\nlast\n"},
		{name: "each marker requires a line", pattern: "...\n...\nlast\n", actual: "one\nlast\n"},
		{name: "retained change", pattern: "first\n...\nlast\n", actual: "changed\none\nlast\n"},
		{name: "anchored start", pattern: "first\n...\n", actual: "extra\nfirst\nlast\n"},
		{name: "anchored end", pattern: "...\nlast\n", actual: "one\nlast\nextra\n"},
		{name: "inline ellipsis is literal", pattern: "a...b\n", actual: "axxxb\n"},
		{name: "indented ellipsis is literal", pattern: " ...\nlast\n", actual: "one\nlast\n"},
		{name: "cannot hide HTTP status", pattern: "...\nbody\n", actual: "HTTP/1.1 503 Service Unavailable\nX-Test: value\n\nbody\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := renderOutputForTest(t, tc.pattern, nil, nil, "stdout")
			current := renderOutputForTest(t, tc.actual, nil, nil, "stdout")
			result, err := reconcileElidedTranscript(old, current, nil, nil)
			require.NoError(t, err)
			if tc.equal {
				require.Equal(t, old, result)
			} else {
				require.Equal(t, current, result, "a mismatch expands the actual capture for review")
			}
		})
	}
}

func TestElisionRetainsVolatileFormatsAndHeaderSemantics(t *testing.T) {
	pattern := "HTTP/1.1 200 OK\nX-Time: 12.3ms\nDate: Mon, 02 Jan 2006 15:04:05 GMT\n...\nSet-Cookie: a=1\nSet-Cookie: b=2\n\nfirst\n...\nlast\n"
	baseline := "HTTP/1.1 200 OK\nSet-Cookie: a=1\nX-Ignored: anything\nDate: Tue, 03 Jan 2006 16:05:06 GMT\nSet-Cookie: b=2\nX-Time: 98.765ms\n\nfirst\nchanging\nmore\nlast\n"
	for _, tc := range []struct {
		name, actual string
		equal        bool
	}{
		{name: "reordered fields and changed values", actual: baseline, equal: true},
		{name: "integer milliseconds", actual: strings.ReplaceAll(baseline, "98.765ms", "2ms"), equal: true},
		{name: "date format", actual: strings.ReplaceAll(baseline, "Tue, 03 Jan 2006 16:05:06 GMT", "2006-01-03T16:05:06Z")},
		{name: "duration unit", actual: strings.ReplaceAll(baseline, "98.765ms", "98.765s")},
		{name: "invalid duration", actual: strings.ReplaceAll(baseline, "98.765ms", "unknown")},
		{name: "header case", actual: strings.ReplaceAll(baseline, "X-Time:", "x-time:")},
		{name: "removed retained header", actual: strings.ReplaceAll(baseline, "X-Time: 98.765ms\n", "")},
		{name: "repeated field order", actual: strings.NewReplacer("a=1", "b=2", "b=2", "a=1").Replace(baseline)},
		{name: "status", actual: strings.ReplaceAll(baseline, "200 OK", "503 Service Unavailable")},
		{name: "retained body", actual: strings.ReplaceAll(baseline, "first\n", "changed\n")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := renderOutputForTest(t, pattern, nil, nil, "stdout")
			current := renderOutputForTest(t, tc.actual, nil, nil, "stdout")
			result, err := reconcileElidedTranscript(old, current, nil, []string{"x-time"})
			require.NoError(t, err)
			if tc.equal {
				require.Equal(t, old, result)
			} else {
				require.Equal(t, current, result)
			}
		})
	}
}

func TestElisionUsesActualHTTPContext(t *testing.T) {
	request := "> GET / HTTP/1.1\n> Host: example.test\n>\n"
	pattern := "...\n< HTTP/1.1 200 OK\n...\n< X-Time: 12.3ms\n<\n"
	current := request + "< HTTP/1.1 200 OK\n< X-Time: 98.765ms\n< X-Other: ignored\n<\n"
	old := renderOutputForTest(t, pattern, nil, nil, "stderr")
	fresh := renderOutputForTest(t, current, nil, nil, "stderr")
	result, err := reconcileElidedTranscript(old, fresh, nil, []string{"x-time"})
	require.NoError(t, err)
	require.Equal(t, old, result)

	old = renderOutputForTest(t, "...\nX-Time: 12ms\n", nil, nil, "stdout")
	fresh = renderOutputForTest(t, "HTTP/1.1 200 OK\n\nX-Time: 98ms\n", nil, nil, "stdout")
	result, err = reconcileElidedTranscript(old, fresh, nil, []string{"x-time"})
	require.NoError(t, err)
	require.Equal(t, fresh, result, "HTTP-shaped body text remains literal")
}

func TestElisionCustomComparisonAndStreamBoundaries(t *testing.T) {
	rules := []extensions.ExampleComparisonRule{{Type: "duration", Pattern: `(?m)^elapsed: (?P<value>\S+)$`}}
	old := renderOutputForTest(t, "...\nelapsed: 12ms\n", nil, nil, "stdout")
	fresh := renderOutputForTest(t, "ignored\nmore\nelapsed: 98.765ms\n", nil, nil, "stdout")
	result, err := reconcileElidedTranscript(old, fresh, rules, nil)
	require.NoError(t, err)
	require.Equal(t, old, result)
	for _, changed := range []string{
		strings.ReplaceAll(fresh, "98.765ms", "98s"),
		strings.ReplaceAll(fresh, "<!-- stdout", "<!-- stderr"),
	} {
		result, err = reconcileElidedTranscript(old, changed, rules, nil)
		require.NoError(t, err)
		require.Equal(t, changed, result)
	}
	command := "```sh\nprintf '...\\n'\n```\n\n"
	result, err = reconcileElidedTranscript(command+old, strings.ReplaceAll(command, "printf", "echo")+fresh, rules, nil)
	require.NoError(t, err)
	require.Contains(t, result, "echo")
	require.Contains(t, result, "ignored\nmore\n")
	_, err = reconcileElidedTranscript(old, fresh, []extensions.ExampleComparisonRule{{Type: "invalid"}}, nil)
	require.Error(t, err)
	_, err = reconcileElidedTranscript(old, fresh, nil, []string{"invalid header"})
	require.Error(t, err)
}

func TestElisionProjectsByteMetadata(t *testing.T) {
	old := renderOutputForTest(t, "first \t\r\n...\nlast", nil, nil, "stdout")
	fresh := renderOutputForTest(t, "first \t\r\nignored\r\nmore \t\nlast", nil, nil, "stdout")
	result, err := reconcileElidedTranscript(old, fresh, nil, nil)
	require.NoError(t, err)
	require.Equal(t, old, result, "omitted line counts, endings and whitespace are insignificant")
	changed := renderOutputForTest(t, "first \t\nignored\r\nmore\nlast", nil, nil, "stdout")
	result, err = reconcileElidedTranscript(old, changed, nil, nil)
	require.NoError(t, err)
	require.NotEqual(t, old, result)
	require.Contains(t, result, "...\n", "retained byte-format changes refresh the selected output")
	require.Contains(t, result, "Line endings: LF,LF")

	full := renderOutputForTest(t, "first\r\nhidden\r\nmore\r\nlast\r\n", nil, nil, "stdout")
	manuallyElided := strings.ReplaceAll(full, "hidden\nmore\n", "...\n")
	result, err = reconcileElidedTranscript(manuallyElided, full, nil, nil)
	require.NoError(t, err)
	require.NotEqual(t, manuallyElided, result, "the first update repairs metadata after editing the output")
	require.Equal(t, renderOutputForTest(t, "first\r\n...\nlast\r\n", nil, nil, "stdout"), result)
	second, err := reconcileElidedTranscript(result, full, nil, nil)
	require.NoError(t, err)
	require.Equal(t, result, second)
}

func TestElisionKeepsIndependentBlocksAndMalformedMetadata(t *testing.T) {
	old := renderOutputForTest(t, "first\n...\nlast\n", nil, nil, "stdout")
	fresh := renderOutputForTest(t, "first\nignored\nlast\n", nil, nil, "stdout")
	old += "\n\n" + renderOutputForTest(t, "original\n", nil, nil, "stderr")
	fresh += "\n\n" + renderOutputForTest(t, "changed\n", nil, nil, "stderr")
	result, err := reconcileElidedTranscript(old, fresh, nil, nil)
	require.NoError(t, err)
	require.Contains(t, result, "first\n...\nlast\n")
	require.Contains(t, result, "changed\n")
	require.NotContains(t, result, "ignored\n")
	for _, metadata := range []string{
		"<!-- stdout; Unknown: value -->\n",
		"<!-- stdout; Line endings: invalid -->\n",
		"<!-- stdout; Line endings: LF; Bare CR offsets: 999 -->\n",
	} {
		previous := fencedText("...\nlast\n", metadata, "```")
		current := renderOutputForTest(t, "ignored\nlast\n", nil, nil, "stdout")
		result, err = reconcileElidedTranscript(previous, current, nil, nil)
		require.NoError(t, err)
		require.NotEqual(t, previous, result, "malformed metadata must produce drift")
		require.Contains(t, result, "<!-- stdout; Line endings: LF,LF -->")
		result, err = reconcileElidedTranscript(current, previous, nil, nil)
		require.NoError(t, err)
		require.Equal(t, previous, result, "invalid fresh captures cannot be projected")
	}
}
