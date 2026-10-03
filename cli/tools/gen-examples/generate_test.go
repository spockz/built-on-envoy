// Copyright Built On Envoy
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

// These tests exercise generated manifest edits and the command surface that users copy from docs.
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/tetratelabs/built-on-envoy/cli/internal/extensions"
)

func TestPatchExampleCodesPreservesOtherManifestText(t *testing.T) {
	source := "# heading\nname: sample\nexamples:\n  - title: First\n    description: keep this\n    code: |- # preserve comment\n      old output\n    # retain nearby note\n  - title: Last\n    commands: []\nextensionSet: true # keep root metadata\n"
	updated, err := patchExampleCodes([]byte(source), map[int]string{0: "new output\nsecond line\n"})
	require.NoError(t, err)
	for _, preserved := range []string{"# heading\n", "description: keep this\n", "# preserve comment\n", "# retain nearby note\n", "extensionSet: true # keep root metadata\n"} {
		require.Contains(t, string(updated), preserved)
	}
	require.NotContains(t, string(updated), "old output")
	var parsed struct {
		Examples []struct {
			Title string `yaml:"title"`
			Code  string `yaml:"code"`
		} `yaml:"examples"`
		ExtensionSet bool `yaml:"extensionSet"`
	}
	require.NoError(t, yaml.Unmarshal(updated, &parsed))
	require.Equal(t, "new output\nsecond line\n", parsed.Examples[0].Code)
	require.Contains(t, string(updated), "    code: | # preserve comment\n      new output\n      second line\n")
	require.True(t, parsed.ExtensionSet)
}

func TestPatchMissingCodeStaysInsideLastExample(t *testing.T) {
	source := "name: sample\nexamples:\n  - title: Last\n    description: entry\n# root comment\nextensionSet: true\n"
	updated, err := patchExampleCodes([]byte(source), map[int]string{0: "generated\n"})
	require.NoError(t, err)
	var parsed struct {
		Examples []struct {
			Code string `yaml:"code"`
		} `yaml:"examples"`
		ExtensionSet bool `yaml:"extensionSet"`
	}
	require.NoError(t, yaml.Unmarshal(updated, &parsed))
	require.Equal(t, "generated\n", parsed.Examples[0].Code)
	require.True(t, parsed.ExtensionSet)
	require.Contains(t, string(updated), "# root comment\nextensionSet: true\n")
}

func TestPatchRejectsFlowStyleExamples(t *testing.T) {
	_, err := patchExampleCodes([]byte("name: sample\nexamples: [{title: one, code: old}]\n"), map[int]string{0: "new\n"})
	require.ErrorContains(t, err, "flow-style")
}

func TestShellRenderingExpandsOnlyDeclaredPlaceholders(t *testing.T) {
	argv := []string{"printf", "%s\\n", "${PROXY_URL}/a?x=1&y=2", "${WORK_DIR}/a'b", "$HOME", "back`tick"}
	line := "set -- " + shellJoin(argv) + "; printf '%s\\n' \"$@\""
	// The test exercises shellJoin's escaping and only invokes fixed printf commands.
	cmd := exec.Command("/bin/sh", "-c", line) // #nosec G204
	cmd.Env = append(os.Environ(), "PROXY_URL=http://127.0.0.1:10000", "WORK_DIR=/tmp/fixture")
	output, err := cmd.Output()
	require.NoError(t, err)
	require.Equal(t, "printf\n%s\\n\nhttp://127.0.0.1:10000/a?x=1&y=2\n/tmp/fixture/a'b\n$HOME\nback`tick\n", string(output))
	_, err = expandString("${lowercase}", map[string]string{})
	require.ErrorContains(t, err, "unknown placeholder")
}

func TestRenderOutputPreservesLineEndingStyleAndTrailingNewline(t *testing.T) {
	values := map[string]string{}
	require.Equal(t, "# one\n# two\n# Line endings: CRLF,LF\n", renderOutput("one\r\ntwo\n", values, false))
	require.Equal(t, "# one\n# Line endings: LF\n# No trailing newline\n", renderOutput("one\n", values, true))
	require.Equal(t, "# first\n#\n# last\n# Line endings: LF,LF,LF\n", renderOutput("first\n\nlast\n", values, false))
}

func TestWaitAdminReportsFakeBoeEarlyExit(t *testing.T) {
	dir := t.TempDir()
	boePath := filepath.Join(dir, "fake-boe")
	require.NoError(t, os.WriteFile(boePath, []byte("#!/bin/sh\nexit 17\n"), 0o600))
	require.NoError(t, os.Chmod(boePath, 0o700)) // #nosec G302 -- the fake BOE must be executable by the process lifecycle test.
	proc, err := startBoe(context.Background(), boePath, nil, dir, dir, dir, dir, dir)
	require.NoError(t, err)
	err = waitAdmin(context.Background(), proc, "127.0.0.1:0", time.Second)
	require.ErrorContains(t, err, "boe exited before Envoy became ready")
	require.Error(t, stopProcess(proc))
}

func TestWriteManifestChangesRefusesConcurrentEdit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest.yaml")
	require.NoError(t, os.WriteFile(path, []byte("changed\n"), 0o600))
	err := writeManifestChanges([]manifestChange{{path: path, old: []byte("original\n"), data: []byte("generated\n")}})
	require.ErrorContains(t, err, "changed while examples were running")
	contents, readErr := os.ReadFile(path) // #nosec G304 -- test reads its own temporary manifest.
	require.NoError(t, readErr)
	require.Equal(t, "changed\n", string(contents))
}

func TestGenerateCheckUpdateAndFailureAtomicity(t *testing.T) {
	extensionPath := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(extensionPath, "examples"), 0o700))
	fixturePath := filepath.Join(extensionPath, "examples", "run.sh")
	require.NoError(t, os.WriteFile(fixturePath, []byte("printf 'HTTP/1.1 200 OK\\r\\nDate: Tue, 03 Jan 2006 16:05:06 GMT\\r\\nX-Test-Date: Tue, 03 Jan 2006 16:05:06 GMT\\r\\nX-Test-Duration: 10ms\\r\\nX-Test-Exact: stable\\r\\n\\r\\nbody\\r\\n'\n"), 0o600))
	boePath := fakeBoe(t)
	upstreamListener, err := net.Listen("tcp", "127.0.0.1:0")
	if errors.Is(err, syscall.EPERM) || (err != nil && strings.Contains(err.Error(), "operation not permitted")) {
		t.Skip("sandbox does not allow loopback listeners")
	}
	require.NoError(t, err)
	upstreamAddress := upstreamListener.Addr().String()
	require.NoError(t, upstreamListener.Close())
	config := map[string]any{}
	manifest := extensions.Manifest{
		Name: "example-test", Version: "1.0.0", Categories: []string{"Examples"},
		Author: "Test", Description: "Example generator test", LongDescription: "Generator test.",
		Type: extensions.TypeLua, Lua: &extensions.Lua{Inline: "function envoy_on_request(handle) end"},
		Tags: []string{"example"}, License: "Apache-2.0",
		Examples: []extensions.Example{{
			Title: "Executable", Description: "Runs a fixture.", Code: "old transcript\n",
			Config: &config, Commands: []extensions.ExampleCommand{{Argv: []string{"sh", "run.sh"}}},
			VolatileHeaders: []string{"x-test-date", "x-test-duration"},
		}},
	}
	writeManifest := func() {
		data, marshalErr := yaml.Marshal(&manifest)
		require.NoError(t, marshalErr)
		require.NoError(t, os.WriteFile(filepath.Join(extensionPath, "manifest.yaml"), data, 0o600))
	}
	manifestPath := filepath.Join(extensionPath, "manifest.yaml")
	writeManifest()
	opts := &options{
		extensions: stringList{extensionPath}, boe: boePath, envoyPath: "/fake/envoy",
		envoyVersion: "1.38.0", timeout: 5 * time.Second, upstreamAddress: upstreamAddress,
	}

	before, err := os.ReadFile(manifestPath) // #nosec G304 -- test reads its own temporary manifest.
	require.NoError(t, err)
	var checkOutput, checkDiff strings.Builder
	opts.check = true
	err = generate(context.Background(), opts, &checkOutput, &checkDiff)
	require.ErrorContains(t, err, "generated examples are stale")
	afterCheck, err := os.ReadFile(manifestPath) // #nosec G304 -- test reads its own temporary manifest.
	require.NoError(t, err)
	require.Equal(t, before, afterCheck)
	require.Contains(t, checkDiff.String(), "generated examples differ")
	require.Contains(t, checkDiff.String(), "# Output:")

	opts.check = false
	var updateOutput, updateErrors strings.Builder
	require.NoError(t, generate(context.Background(), opts, &updateOutput, &updateErrors))
	generated, err := os.ReadFile(manifestPath) // #nosec G304 -- test reads its own temporary manifest.
	require.NoError(t, err)
	require.Contains(t, string(generated), "Date: Tue, 03 Jan 2006 16:05:06 GMT")
	require.Contains(t, string(generated), "# Line endings: CRLF,CRLF,CRLF,CRLF,CRLF,CRLF,CRLF\n")
	require.NoError(t, yaml.Unmarshal(generated, &manifest))
	beforeEquivalent, err := os.ReadFile(manifestPath) // #nosec G304 -- test reads its own temporary manifest.
	require.NoError(t, err)
	fixture := "printf 'HTTP/1.1 200 OK\\r\\nDate: Wed, 04 Jan 2006 17:06:07 GMT\\r\\nX-Test-Date: Wed, 04 Jan 2006 17:06:07 GMT\\r\\nX-Test-Duration: 25ms\\r\\nX-Test-Exact: stable\\r\\n\\r\\nbody\\r\\n'\n"
	require.NoError(t, os.WriteFile(fixturePath, []byte(fixture), 0o600))
	var equalOutput, equalErrors strings.Builder
	opts.check = true
	require.NoError(t, generate(context.Background(), opts, &equalOutput, &equalErrors))
	afterEquivalent, err := os.ReadFile(manifestPath) // #nosec G304 -- test reads its own temporary manifest.
	require.NoError(t, err)
	require.Equal(t, beforeEquivalent, afterEquivalent, "volatile response headers should preserve the reviewed transcript")
	require.Contains(t, equalOutput.String(), "generated examples are current")

	require.NoError(t, yaml.Unmarshal(beforeEquivalent, &manifest))
	require.NoError(t, os.WriteFile(fixturePath, []byte(strings.ReplaceAll(fixture, "25ms", "25.5 ms")), 0o600))
	beforeFormatChange, err := os.ReadFile(manifestPath) // #nosec G304 -- test reads its own temporary manifest.
	require.NoError(t, err)
	var formatOutput, formatDiff strings.Builder
	require.ErrorContains(t, generate(context.Background(), opts, &formatOutput, &formatDiff), "generated examples are stale")
	afterFormatChange, err := os.ReadFile(manifestPath) // #nosec G304 -- test reads its own temporary manifest.
	require.NoError(t, err)
	require.Equal(t, beforeFormatChange, afterFormatChange, "format changes should be reported without writing in check mode")

	manifest.Examples[0].Code = "old first transcript\n"
	manifest.Examples = append(manifest.Examples, extensions.Example{
		Title: "Fails later", Description: "Failure should not partially update the manifest.", Code: "old second transcript\n",
		Config:   &config,
		Commands: []extensions.ExampleCommand{{Argv: []string{"sh", "-c", "exit 5"}}},
	})
	writeManifest()
	beforeFailure, err := os.ReadFile(manifestPath) // #nosec G304 -- test reads its own temporary manifest.
	require.NoError(t, err)
	opts.check = false
	err = generate(context.Background(), opts, &strings.Builder{}, &strings.Builder{})
	require.ErrorContains(t, err, `command "sh -c 'exit 5'" exited 5, expected 0`)
	afterFailure, err := os.ReadFile(manifestPath) // #nosec G304 -- test reads its own temporary manifest.
	require.NoError(t, err)
	require.Equal(t, beforeFailure, afterFailure, "a later command failure must prevent all manifest writes")
}

func fakeBoe(t *testing.T) string {
	t.Helper()
	executable, err := os.Executable()
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "fake-boe")
	script := fmt.Sprintf("#!/bin/sh\nGO_WANT_GEN_EXAMPLES_HELPER=1 exec %s -test.run=TestGenExamplesHelperProcess\n", shellQuote(executable))
	require.NoError(t, os.WriteFile(path, []byte(script), 0o600))
	require.NoError(t, os.Chmod(path, 0o700)) // #nosec G302 -- the test helper wrapper must be executable by the generator.
	return path
}

func TestGenExamplesHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_GEN_EXAMPLES_HELPER") != "1" {
		return
	}
	address := os.Getenv("BOE_ADMIN_ADDRESS")
	http.HandleFunc("/ready", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "LIVE")
	})
	server := &http.Server{Addr: address, Handler: http.DefaultServeMux, ReadHeaderTimeout: time.Second}
	if err := server.ListenAndServe(); err != nil {
		t.Fatal(err)
	}
}
