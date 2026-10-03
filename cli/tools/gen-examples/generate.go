// Copyright Built On Envoy
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

// This file owns the example run lifecycle and source-preserving manifest update so generated
// transcripts can be refreshed without reformatting authors' surrounding YAML.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mccutchen/go-httpbin/v2/httpbin"
	"github.com/pmezard/go-difflib/difflib"
	"gopkg.in/yaml.v3"

	"github.com/tetratelabs/built-on-envoy/cli/internal/extensions"
)

var placeholderPattern = regexp.MustCompile(`\$\{([^}]+)\}`)

type manifestChange struct {
	path string
	old  []byte
	data []byte
}

func generate(ctx context.Context, opts *options, stdout, stderr io.Writer) (returnErr error) {
	if opts.timeout <= 0 {
		return fmt.Errorf("timeout must be positive")
	}
	root, rootErr := moduleRoot()
	if rootErr != nil {
		return rootErr
	}
	if len(opts.extensions) == 0 {
		server, serveErr := startUpstream(opts.upstreamAddress)
		if serveErr != nil {
			return serveErr
		}
		if _, writeErr := fmt.Fprintf(stdout, "httpbin listening on %s\n", opts.upstreamAddress); writeErr != nil {
			return fmt.Errorf("write upstream address: %w", writeErr)
		}
		select {
		case <-ctx.Done():
			return stopUpstream(server)
		case <-server.done:
			return stopUpstream(server)
		}
	}
	type extensionInput struct {
		path     string
		manifest *extensions.Manifest
		raw      []byte
		examples []extensions.Example
	}
	inputs := make([]extensionInput, 0, len(opts.extensions))
	hasExecutable := false
	for _, extension := range opts.extensions {
		path, pathErr := filepath.Abs(extension)
		if pathErr != nil {
			return fmt.Errorf("resolve extension directory %s: %w", extension, pathErr)
		}
		manifestPath := filepath.Join(path, "manifest.yaml")
		manifest, manifestErr := extensions.LoadLocalManifest(manifestPath)
		if manifestErr != nil {
			return fmt.Errorf("load extension manifest %s: %w", manifestPath, manifestErr)
		}
		// The directory and manifest are explicit generator inputs supplied by the extension build.
		raw, readErr := os.ReadFile(manifestPath) // #nosec G304
		if readErr != nil {
			return fmt.Errorf("read extension manifest: %w", readErr)
		}
		var sourceManifest struct {
			Examples []extensions.Example `yaml:"examples"`
		}
		if decodeErr := yaml.Unmarshal(raw, &sourceManifest); decodeErr != nil {
			return fmt.Errorf("decode extension examples: %w", decodeErr)
		}
		if len(sourceManifest.Examples) != len(manifest.Examples) {
			return fmt.Errorf("manifest example count changed while loading")
		}
		manifest.Path = manifestPath
		for exampleIndex := range sourceManifest.Examples {
			example := &sourceManifest.Examples[exampleIndex]
			if _, compareErr := equivalentTranscript("", "", example.Comparison, example.VolatileHeaders); compareErr != nil {
				return fmt.Errorf("invalid comparison rules in %s example %q: %w", manifestPath, example.Title, compareErr)
			}
		}
		for exampleIndex := range sourceManifest.Examples {
			hasExecutable = hasExecutable || len(sourceManifest.Examples[exampleIndex].Commands) > 0
		}
		inputs = append(inputs, extensionInput{path: path, manifest: manifest, raw: raw, examples: sourceManifest.Examples})
	}
	if !hasExecutable {
		if _, printErr := fmt.Fprintln(stdout, "no executable examples found"); printErr != nil {
			return fmt.Errorf("write generator result: %w", printErr)
		}
		return nil
	}
	boePath, cleanupBuild, err := resolveBoe(root, opts.boe)
	if err != nil {
		return err
	}
	defer cleanupBuild()

	var upstream *upstreamServer
	if hasExecutable {
		upstream, err = startUpstream(opts.upstreamAddress)
		if err != nil {
			return err
		}
		defer func() {
			if stopErr := stopUpstream(upstream); stopErr != nil && returnErr == nil {
				returnErr = stopErr
			}
		}()
	}

	sharedData, err := os.MkdirTemp("", "boe-example-data-*")
	if err != nil {
		return fmt.Errorf("create isolated extension data directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(sharedData) }()
	if opts.envoyPath == "" {
		if cacheErr := prepareEnvoyCache(root, sharedData, opts.envoyVersion); cacheErr != nil {
			return fmt.Errorf("prepare Envoy download cache: %w", cacheErr)
		}
	}
	var changed []manifestChange
	stale := false
	for _, input := range inputs {
		updates := make(map[int]string)
		executed := false
		for i := range input.examples {
			example := &input.examples[i]
			if len(example.Commands) == 0 {
				continue
			}
			executed = true
			code, err := runExample(ctx, root, opts, input.path, input.manifest, example, boePath, sharedData)
			if err != nil {
				return fmt.Errorf("extension %q example %q: %w", input.manifest.Name, example.Title, err)
			}
			if code != example.Code {
				updates[i] = code
			}
		}
		if len(updates) == 0 {
			if executed {
				if _, err := fmt.Fprintf(stdout, "%s: generated examples are current\n", input.manifest.Name); err != nil {
					return fmt.Errorf("write generator result: %w", err)
				}
			}
			continue
		}
		updated, err := patchExampleCodes(input.raw, updates)
		if err != nil {
			return err
		}
		var updatedManifest extensions.Manifest
		if err := yaml.Unmarshal(updated, &updatedManifest); err != nil {
			return fmt.Errorf("decode updated manifest %s: %w", input.manifest.Path, err)
		}
		if err := extensions.ValidateManifest(&updatedManifest); err != nil {
			return fmt.Errorf("validate updated manifest %s: %w", input.manifest.Path, err)
		}
		if !bytes.Equal(input.raw, updated) {
			stale = true
			changed = append(changed, manifestChange{path: input.manifest.Path, old: input.raw, data: updated})
			if opts.check {
				if _, err := fmt.Fprintf(stderr, "generated examples differ in %s\n", input.manifest.Path); err != nil {
					return fmt.Errorf("write stale example message: %w", err)
				}
				if err := writeUnifiedDiff(stderr, string(input.raw), string(updated)); err != nil {
					return fmt.Errorf("write example diff: %w", err)
				}
			}
		} else {
			if _, err := fmt.Fprintf(stdout, "%s: generated examples are current\n", input.manifest.Name); err != nil {
				return fmt.Errorf("write generator result: %w", err)
			}
		}
	}
	if opts.check && stale {
		return fmt.Errorf("generated examples are stale")
	}
	if err := writeManifestChanges(changed); err != nil {
		return err
	}
	for _, item := range changed {
		if _, err := fmt.Fprintf(stdout, "%s: updated generated examples\n", item.path); err != nil {
			return fmt.Errorf("write generator result: %w", err)
		}
	}
	return nil
}

func writeManifestChanges(changes []manifestChange) error {
	type stagedFile struct{ target, temp string }
	staged := make([]stagedFile, 0, len(changes))
	defer func() {
		for _, file := range staged {
			_ = os.Remove(file.temp)
		}
	}()
	for _, change := range changes {
		// This is the exact manifest path loaded and validated at the start of generation.
		current, err := os.ReadFile(change.path) // #nosec G304
		if err != nil {
			return fmt.Errorf("re-read manifest %s before update: %w", change.path, err)
		}
		if !bytes.Equal(current, change.old) {
			return fmt.Errorf("manifest %s changed while examples were running; refusing to overwrite it", change.path)
		}
		info, err := os.Stat(change.path)
		if err != nil {
			return fmt.Errorf("inspect manifest %s before update: %w", change.path, err)
		}
		file, err := os.CreateTemp(filepath.Dir(change.path), ".manifest-*.tmp")
		if err != nil {
			return fmt.Errorf("stage manifest update for %s: %w", change.path, err)
		}
		staged = append(staged, stagedFile{target: change.path, temp: file.Name()})
		if err := file.Chmod(info.Mode().Perm()); err != nil {
			_ = file.Close()
			return fmt.Errorf("set permissions on staged manifest %s: %w", change.path, err)
		}
		if _, err := file.Write(change.data); err != nil {
			_ = file.Close()
			return fmt.Errorf("write staged manifest %s: %w", change.path, err)
		}
		if err := file.Sync(); err != nil {
			_ = file.Close()
			return fmt.Errorf("sync staged manifest %s: %w", change.path, err)
		}
		if err := file.Close(); err != nil {
			return fmt.Errorf("close staged manifest %s: %w", change.path, err)
		}
	}
	for _, file := range staged {
		if err := os.Rename(file.temp, file.target); err != nil {
			return fmt.Errorf("replace manifest %s: %w", file.target, err)
		}
	}
	return nil
}

func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get working directory: %w", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			if _, err := os.Stat(filepath.Join(dir, "cli", "main.go")); err == nil {
				return dir, nil
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("could not find repository root above %s", dir)
		}
		dir = parent
	}
}

func resolveBoe(root, provided string) (string, func(), error) {
	if provided != "" {
		path, err := filepath.Abs(provided)
		return path, func() {}, err
	}
	dir, err := os.MkdirTemp("", "boe-example-cli-*")
	if err != nil {
		return "", nil, fmt.Errorf("create temporary build directory: %w", err)
	}
	path := filepath.Join(dir, "boe")
	if runtime.GOOS == "windows" {
		path += ".exe"
	}
	// The executable and arguments are fixed; extension content is not interpolated into this build.
	cmd := exec.Command("go", "build", "-o", path, "./cli") // #nosec G204
	cmd.Dir = root
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Run(); err != nil {
		_ = os.RemoveAll(dir)
		return "", nil, fmt.Errorf("build boe from current checkout: %w\n%s", err, output.String())
	}
	return path, func() { _ = os.RemoveAll(dir) }, nil
}

type upstreamServer struct {
	server *http.Server
	done   chan struct{}
	err    error
}

func startUpstream(address string) (*upstreamServer, error) {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, fmt.Errorf("listen for local httpbin upstream at %s: %w", address, err)
	}
	server := &http.Server{Handler: httpbin.New(), ReadHeaderTimeout: 5 * time.Second}
	upstream := &upstreamServer{server: server, done: make(chan struct{})}
	go func() {
		upstream.err = server.Serve(listener)
		if errors.Is(upstream.err, http.ErrServerClosed) {
			upstream.err = nil
		}
		close(upstream.done)
	}()
	return upstream, nil
}

func stopUpstream(upstream *upstreamServer) error {
	if err := upstream.server.Close(); err != nil {
		return fmt.Errorf("stop local httpbin upstream: %w", err)
	}
	<-upstream.done
	if upstream.err != nil {
		return fmt.Errorf("local httpbin upstream failed: %w", upstream.err)
	}
	return nil
}

func runExample(ctx context.Context, root string, opts *options, extensionPath string, manifest *extensions.Manifest, example *extensions.Example, boe, sharedData string) (result string, returnErr error) {
	workDir, err := os.MkdirTemp("", "boe-example-*")
	if err != nil {
		return "", fmt.Errorf("create temporary working directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(workDir) }()
	if fixtureErr := stageFixtures(extensionPath, workDir); fixtureErr != nil {
		return "", fixtureErr
	}
	boeHome := filepath.Join(workDir, ".boe")
	configHome := filepath.Join(boeHome, "config")
	dataHome := sharedData
	stateHome := filepath.Join(boeHome, "state")
	runtimeDir := filepath.Join(boeHome, "runtime")
	for _, dir := range []string{configHome, stateHome, runtimeDir} {
		if mkdirErr := os.MkdirAll(dir, 0o700); mkdirErr != nil {
			return "", fmt.Errorf("create isolated BOE directory: %w", mkdirErr)
		}
	}
	proxyListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("allocate Envoy listener port: %w", err)
	}
	adminListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = proxyListener.Close()
		return "", fmt.Errorf("allocate Envoy admin port: %w", err)
	}
	proxyAddr, adminAddr := proxyListener.Addr().String(), adminListener.Addr().String()
	defer func() {
		if closeErr := proxyListener.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) && returnErr == nil {
			returnErr = fmt.Errorf("release reserved Envoy listener port: %w", closeErr)
		}
		if closeErr := adminListener.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) && returnErr == nil {
			returnErr = fmt.Errorf("release reserved Envoy admin port: %w", closeErr)
		}
	}()
	values := map[string]string{
		"PROXY_URL":        "http://" + proxyAddr,
		"ADMIN_URL":        "http://" + adminAddr,
		"UPSTREAM_ADDRESS": opts.upstreamAddress,
		"WORK_DIR":         workDir,
	}
	configJSON := ""
	if example.Config != nil {
		config, configErr := expandMap(*example.Config, values)
		if configErr != nil {
			return "", fmt.Errorf("expand config: %w", configErr)
		}
		b, configErr := json.Marshal(config)
		if configErr != nil {
			return "", fmt.Errorf("encode config: %w", configErr)
		}
		configJSON = string(b)
	}
	args := []string{"run", "--local", filepath.Dir(manifest.Path), "--listen-port", portOf(proxyAddr), "--admin-port", portOf(adminAddr)}
	if opts.envoyPath != "" {
		args = append(args, "--envoy-path", opts.envoyPath)
	} else {
		args = append(args, "--envoy-version", opts.envoyVersion)
	}
	args = append(args, "--cluster-insecure", opts.upstreamAddress, "--test-upstream-cluster", opts.upstreamAddress)
	if example.Config != nil {
		args = append(args, "--config", configJSON)
	}
	transcriptArgs := append([]string(nil), args...)
	for i, arg := range transcriptArgs {
		if arg == "--listen-port" && i+1 < len(transcriptArgs) {
			transcriptArgs[i+1] = "${PROXY_URL##*:}"
		}
		if arg == "--admin-port" && i+1 < len(transcriptArgs) {
			transcriptArgs[i+1] = "${ADMIN_URL##*:}"
		}
		if (arg == "--cluster-insecure" || arg == "--test-upstream-cluster") && i+1 < len(transcriptArgs) {
			transcriptArgs[i+1] = "${UPSTREAM_ADDRESS}"
		}
	}
	for i := range transcriptArgs {
		if transcriptArgs[i] == "--envoy-path" && i+1 < len(transcriptArgs) {
			transcriptArgs[i+1] = "${ENVOY_PATH}"
		}
	}
	commandText := "$ boe " + shellJoin(transcriptArgs)
	// The source transcript uses a portable local path; execution uses the absolute path.
	relExtension, err := filepath.Rel(root, filepath.Dir(manifest.Path))
	if err != nil {
		return "", fmt.Errorf("make extension path portable: %w", err)
	}
	commandText = strings.Replace(commandText, shellQuote(filepath.Dir(manifest.Path)), shellQuote(relExtension), 1)
	if example.Config != nil {
		stable, stableErr := stableJSON(*example.Config)
		if stableErr != nil {
			return "", fmt.Errorf("encode stable example config: %w", stableErr)
		}
		commandText = strings.Replace(commandText, shellQuote(configJSON), shellQuote(stable), 1)
	}
	var transcript strings.Builder
	transcript.WriteString("# Local upstream (started by this generator)\n")
	transcript.WriteString("# Run the following commands from the repository root.\n")
	transcript.WriteString("$ export PROXY_URL='http://127.0.0.1:10000'\n")
	transcript.WriteString("$ export ADMIN_URL='http://127.0.0.1:9901'\n")
	transcript.WriteString("$ export UPSTREAM_ADDRESS='127.0.0.1:10001'\n")
	transcript.WriteString("$ export WORK_DIR=\"$(mktemp -d)\"\n")
	if opts.envoyPath != "" {
		transcript.WriteString("$ export ENVOY_PATH='/path/to/envoy'\n")
		transcript.WriteString("# Set ENVOY_PATH to a compatible Envoy binary before starting Envoy.\n")
	}
	transcript.WriteString("$ go run ./cli/tools/gen-examples -serve-upstream -upstream-address \"${UPSTREAM_ADDRESS}\" &\n")
	transcript.WriteString("$ UPSTREAM_PID=$!\n")
	if hasFixtures(extensionPath) {
		transcript.WriteString("\n# Copy fixture files from extensions/<extension>/examples/ into ${WORK_DIR} before running commands.\n")
		transcript.WriteString("$ cp -R " + shellQuote(filepath.ToSlash(filepath.Join(relExtension, "examples"))+"/.") + " ${WORK_DIR}/\n")
	}
	transcript.WriteString("\n# Start Envoy\n")
	transcript.WriteString(commandText + " &\n")
	transcript.WriteString("$ BOE_PID=$!\n")
	transcript.WriteString("$ for i in $(seq 1 300); do\n")
	transcript.WriteString("    [ \"$(curl --silent --max-time 1 \"${ADMIN_URL}/ready\" || true)\" = LIVE ] && break\n")
	transcript.WriteString("    sleep 0.1\n")
	transcript.WriteString("  done\n")
	transcript.WriteString("$ cd \"${WORK_DIR}\"\n\n")

	_ = proxyListener.Close()
	_ = adminListener.Close()
	proc, err := startBoe(ctx, boe, args, root, configHome, dataHome, stateHome, runtimeDir)
	if err != nil {
		return "", err
	}
	defer func() {
		if stopErr := stopProcess(proc); stopErr != nil && returnErr == nil {
			returnErr = fmt.Errorf("stop boe process: %w", stopErr)
		}
	}()
	if waitErr := waitAdmin(ctx, proc, adminAddr, opts.timeout); waitErr != nil {
		return "", waitErr
	}

	for i, command := range example.Commands {
		argv, argvErr := expandArgv(command.Argv, values)
		if argvErr != nil {
			return "", fmt.Errorf("command %d: %w", i+1, argvErr)
		}
		if len(argv) == 0 {
			return "", fmt.Errorf("command %d has empty argv", i+1)
		}
		transcript.WriteString("$ " + shellJoin(command.Argv) + "\n")
		cmdCtx, cancel := context.WithTimeout(ctx, opts.timeout)
		// Command argv comes from the executable examples explicitly selected by the extension author.
		cmd := exec.CommandContext(cmdCtx, argv[0], argv[1:]...) // #nosec G204
		cmd.Dir = workDir
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Env = append(os.Environ(), "PROXY_URL="+values["PROXY_URL"], "ADMIN_URL="+values["ADMIN_URL"], "UPSTREAM_ADDRESS="+values["UPSTREAM_ADDRESS"], "WORK_DIR="+workDir)
		var stdoutBuf, stderrBuf bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdoutBuf, &stderrBuf
		runErr := cmd.Run()
		if cmd.Process != nil {
			if killErr := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); killErr != nil && !errors.Is(killErr, syscall.ESRCH) && runErr == nil {
				runErr = fmt.Errorf("clean up command process group: %w", killErr)
			}
		}
		cancel()
		actualExit := exitCode(runErr)
		if runErr != nil && actualExit < 0 {
			return "", fmt.Errorf("command %q failed: %w", shellJoin(command.Argv), runErr)
		}
		if actualExit != command.ExpectedExit {
			mismatch := fmt.Sprintf("command %q exited %d, expected %d", shellJoin(command.Argv), actualExit, command.ExpectedExit)
			if runErr != nil {
				return "", fmt.Errorf("%s: %w", mismatch, runErr)
			}
			return "", errors.New(mismatch)
		}
		if stdoutBuf.Len() > 0 {
			transcript.WriteString("# Output:\n")
			transcript.WriteString(renderOutput(stdoutBuf.String(), values, !strings.HasSuffix(stdoutBuf.String(), "\n")))
			transcript.WriteString("# End output\n")
		}
		if stderrBuf.Len() > 0 {
			transcript.WriteString("# Stderr:\n")
			transcript.WriteString(renderOutput(stderrBuf.String(), values, !strings.HasSuffix(stderrBuf.String(), "\n")))
			transcript.WriteString("# End output\n")
		}
		transcript.WriteString("\n")
	}
	transcript.WriteString("$ cd - >/dev/null\n")
	transcript.WriteString("$ kill \"${BOE_PID}\" \"${UPSTREAM_PID}\"\n")
	transcript.WriteString("$ rm -rf \"${WORK_DIR}\"\n")
	oldCode := example.Code
	equal, err := equivalentTranscript(oldCode, transcript.String(), example.Comparison, example.VolatileHeaders)
	if err != nil {
		return "", fmt.Errorf("compare generated transcript: %w", err)
	}
	if equal {
		return oldCode, nil
	}
	return transcript.String(), nil
}

type lockedBuffer struct {
	mu sync.Mutex
	bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.String()
}

type boeProcess struct {
	cmd     *exec.Cmd
	done    chan struct{}
	waitErr error
	stdout  *lockedBuffer
	stderr  *lockedBuffer
}

func startBoe(ctx context.Context, boe string, args []string, root, configHome, dataHome, stateHome, runtimeDir string) (*boeProcess, error) {
	cmd := exec.CommandContext(ctx, boe, args...)
	cmd.Dir = root
	cmd.Env = replaceEnv(os.Environ(), map[string]string{
		"BOE_CONFIG_HOME":   configHome,
		"BOE_DATA_HOME":     dataHome,
		"BOE_STATE_HOME":    stateHome,
		"BOE_RUNTIME_DIR":   runtimeDir,
		"BOE_RUN_ID":        "examples",
		"BOE_RUN_DOCKER":    "false",
		"BOE_ADMIN_ADDRESS": "127.0.0.1:" + portOfFromAdmin(args),
		"ENVOY_PATH":        "",
		"ENVOY_VERSION":     "",
	})
	stdout, stderr := &lockedBuffer{}, &lockedBuffer{}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start boe: %w", err)
	}
	proc := &boeProcess{cmd: cmd, done: make(chan struct{}), stdout: stdout, stderr: stderr}
	go func() {
		proc.waitErr = cmd.Wait()
		close(proc.done)
	}()
	return proc, nil
}

func replaceEnv(existing []string, replacements map[string]string) []string {
	result := make([]string, 0, len(existing)+len(replacements))
	for _, item := range existing {
		key, _, found := strings.Cut(item, "=")
		if !found {
			result = append(result, item)
			continue
		}
		if _, replaced := replacements[key]; replaced {
			continue
		}
		result = append(result, item)
	}
	keys := make([]string, 0, len(replacements))
	for key := range replacements {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		result = append(result, key+"="+replacements[key])
	}
	return result
}

func portOfFromAdmin(args []string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--admin-port" {
			return args[i+1]
		}
	}
	return "9901"
}

func waitAdmin(ctx context.Context, proc *boeProcess, adminAddress string, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	client := &http.Client{Timeout: 250 * time.Millisecond}
	url := "http://" + adminAddress + "/ready"
	for {
		select {
		case <-proc.done:
			return fmt.Errorf("boe exited before Envoy became ready: %w; stderr: %s", proc.waitErr, proc.stderr.String())
		default:
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("envoy did not become ready at %s within %s; boe stdout: %s; stderr: %s", url, timeout, proc.stdout.String(), proc.stderr.String())
		case <-proc.done:
			return fmt.Errorf("boe exited before Envoy became ready: %w; stderr: %s", proc.waitErr, proc.stderr.String())
		case <-ticker.C:
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
			if err != nil {
				return err
			}
			resp, err := client.Do(req)
			if err != nil {
				select {
				case <-proc.done:
					return fmt.Errorf("boe exited before Envoy became ready: %w; stderr: %s", proc.waitErr, proc.stderr.String())
				default:
				}
				continue
			}
			body, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if readErr == nil && resp.StatusCode == http.StatusOK && strings.EqualFold(strings.TrimSpace(string(body)), "live") {
				return nil
			}
		}
	}
}

func stopProcess(proc *boeProcess) error {
	if proc == nil || proc.cmd == nil || proc.cmd.Process == nil {
		return nil
	}
	groupID := -proc.cmd.Process.Pid
	if err := syscall.Kill(groupID, syscall.SIGINT); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("interrupt boe process group: %w", err)
	}
	select {
	case <-proc.done:
		if err := syscall.Kill(groupID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("clean up boe process group: %w", err)
		}
		return expectedSignalExit(proc.waitErr)
	case <-time.After(3 * time.Second):
		if err := syscall.Kill(groupID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("force stop boe process group: %w", err)
		}
		<-proc.done
		return expectedSignalExit(proc.waitErr)
	}
}

func expectedSignalExit(err error) error {
	if err == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ProcessState != nil {
		status, ok := exitErr.Sys().(syscall.WaitStatus)
		if ok && status.Signaled() && (status.Signal() == syscall.SIGINT || status.Signal() == syscall.SIGKILL) {
			return nil
		}
	}
	return err
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

func stageFixtures(extension, workDir string) error {
	source := filepath.Join(extension, "examples")
	info, err := os.Stat(source)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect examples fixture directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("examples fixture path %s is not a directory", source)
	}
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("fixture symlinks are not supported: %s", path)
		}
		dest := filepath.Join(workDir, rel)
		if entry.IsDir() {
			return os.MkdirAll(dest, 0o700)
		}
		// Fixtures are explicitly staged from the selected extension's examples directory.
		data, err := os.ReadFile(path) // #nosec G304
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		mode := info.Mode().Perm() & 0o700
		if mode == 0 {
			mode = 0o600
		}
		return os.WriteFile(dest, data, mode)
	})
}

func hasFixtures(extension string) bool {
	info, err := os.Stat(filepath.Join(extension, "examples"))
	return err == nil && info.IsDir()
}

func expandArgv(argv []string, values map[string]string) ([]string, error) {
	result := make([]string, len(argv))
	for i, arg := range argv {
		value, err := expandString(arg, values)
		if err != nil {
			return nil, err
		}
		result[i] = value
	}
	return result, nil
}

func expandMap(input map[string]any, values map[string]string) (map[string]any, error) {
	result := make(map[string]any, len(input))
	for key, value := range input {
		expanded, err := expandValue(value, values)
		if err != nil {
			return nil, fmt.Errorf("config field %q: %w", key, err)
		}
		result[key] = expanded
	}
	return result, nil
}

func expandValue(value any, values map[string]string) (any, error) {
	switch typed := value.(type) {
	case string:
		return expandString(typed, values)
	case map[string]any:
		return expandMap(typed, values)
	case []any:
		result := make([]any, len(typed))
		for i, item := range typed {
			expanded, err := expandValue(item, values)
			if err != nil {
				return nil, err
			}
			result[i] = expanded
		}
		return result, nil
	default:
		return value, nil
	}
}

func expandString(value string, values map[string]string) (string, error) {
	var expansionErr error
	result := placeholderPattern.ReplaceAllStringFunc(value, func(match string) string {
		if expansionErr != nil {
			return match
		}
		key := placeholderPattern.FindStringSubmatch(match)[1]
		replacement, ok := values[key]
		if !ok {
			expansionErr = fmt.Errorf("unknown placeholder %s", match)
			return match
		}
		return replacement
	})
	return result, expansionErr
}

func renderOutput(value string, values map[string]string, noTrailingNewline bool) string {
	for key, replacement := range values {
		value = strings.ReplaceAll(value, replacement, "${"+key+"}")
	}
	lines := strings.SplitAfter(value, "\n")
	if strings.HasSuffix(value, "\n") {
		lines = lines[:len(lines)-1]
	}
	var out strings.Builder
	lineEndings := make([]string, 0, len(lines))
	for _, line := range lines {
		ending := ""
		if strings.HasSuffix(line, "\r\n") {
			ending = "CRLF"
			line = strings.TrimSuffix(line, "\r\n")
		} else if strings.HasSuffix(line, "\n") {
			ending = "LF"
			line = strings.TrimSuffix(line, "\n")
		}
		if ending != "" {
			lineEndings = append(lineEndings, ending)
		}
		if line == "" {
			out.WriteString("#\n")
		} else {
			out.WriteString("# " + strings.ReplaceAll(line, "\r", `\r`) + "\n")
		}
	}
	if len(lineEndings) > 0 {
		out.WriteString("# Line endings: " + strings.Join(lineEndings, ",") + "\n")
	}
	if noTrailingNewline {
		out.WriteString("# No trailing newline\n")
	}
	return out.String()
}

func shellJoin(argv []string) string {
	quoted := make([]string, len(argv))
	for i, arg := range argv {
		quoted[i] = shellQuote(arg)
	}
	return strings.Join(quoted, " ")
}

func shellQuote(arg string) string {
	if strings.Contains(arg, "${") {
		matches := placeholderPattern.FindAllStringIndex(arg, -1)
		var out strings.Builder
		cursor := 0
		for _, match := range matches {
			if cursor < match[0] {
				out.WriteString(shellQuoteLiteral(arg[cursor:match[0]]))
			}
			out.WriteString("\"" + arg[match[0]:match[1]] + "\"")
			cursor = match[1]
		}
		if cursor < len(arg) {
			out.WriteString(shellQuoteLiteral(arg[cursor:]))
		}
		return out.String()
	}
	return shellQuoteLiteral(arg)
}

func shellQuoteLiteral(arg string) string {
	if arg != "" && strings.IndexFunc(arg, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && !strings.ContainsRune("_./:-", r)
	}) == -1 {
		return arg
	}
	return "'" + strings.ReplaceAll(arg, "'", "'\"'\"'") + "'"
}

func stableJSON(config map[string]any) (string, error) {
	encoded, err := json.Marshal(config)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func portOf(address string) string {
	_, port, err := net.SplitHostPort(address)
	if err == nil {
		return port
	}
	return ""
}

func patchExampleCodes(raw []byte, updates map[int]string) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse source manifest for code updates: %w", err)
	}
	if len(doc.Content) == 0 {
		return nil, fmt.Errorf("manifest is empty")
	}
	root := doc.Content[0]
	examples := mappingValue(root, "examples")
	if examples == nil || examples.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("manifest examples must be a sequence")
	}
	if examples.Style&yaml.FlowStyle != 0 {
		return nil, fmt.Errorf("cannot safely update flow-style examples; use block-style YAML")
	}
	lines := strings.SplitAfter(string(raw), "\n")
	type edit struct {
		start, end  int
		replacement string
	}
	edits := make([]edit, 0, len(updates))
	for index, code := range updates {
		if index < 0 || index >= len(examples.Content) {
			return nil, fmt.Errorf("example index %d is out of range", index)
		}
		entry := examples.Content[index]
		if entry.Style&yaml.FlowStyle != 0 {
			return nil, fmt.Errorf("cannot safely update flow-style example %d; use block-style YAML", index+1)
		}
		codeKey, _ := mappingPair(entry, "code")
		if codeKey != nil {
			start := codeKey.Line - 1
			indent := codeKey.Column - 1
			end := scalarBlockEnd(lines, start, indent)
			header := strings.TrimSuffix(strings.TrimSuffix(lines[start], "\n"), "\r")
			colon := strings.IndexByte(header, ':')
			if colon < 0 {
				return nil, fmt.Errorf("invalid code key at line %d", codeKey.Line)
			}
			comment := ""
			_, codeValue := mappingPair(entry, "code")
			if codeValue != nil && (codeValue.Style&yaml.LiteralStyle != 0 || codeValue.Style&yaml.FoldedStyle != 0) {
				if commentIndex := strings.Index(header[colon+1:], "#"); commentIndex >= 0 {
					comment = " " + strings.TrimSpace(header[colon+1+commentIndex:])
				}
			}
			prefix := header[:colon+1] + " |" + comment + "\n"
			replacement := prefix + renderScalarContent(code, indent+2)
			edits = append(edits, edit{start: start, end: end, replacement: replacement})
			continue
		}
		entryLine := entry.Line - 1
		indent := sequenceItemIndent(lines, entryLine) + 2
		insertAt := entryEndLine(lines, entryLine, sequenceItemIndent(lines, entryLine))
		edits = append(edits, edit{start: insertAt, end: insertAt, replacement: strings.Repeat(" ", indent) + "code: |\n" + renderScalarContent(code, indent+2)})
	}
	sort.Slice(edits, func(i, j int) bool { return edits[i].start > edits[j].start })
	for _, e := range edits {
		if e.start < 0 || e.end < e.start || e.end > len(lines) {
			return nil, fmt.Errorf("invalid code update range")
		}
		before := append([]string(nil), lines[:e.start]...)
		after := append([]string(nil), lines[e.end:]...)
		middle := strings.SplitAfter(e.replacement, "\n")
		if len(middle) > 0 && middle[len(middle)-1] == "" {
			middle = middle[:len(middle)-1]
		}
		lines = append(append(before, middle...), after...)
	}
	return []byte(strings.Join(lines, "")), nil
}

func mappingValue(node *yaml.Node, key string) *yaml.Node {
	_, value := mappingPair(node, key)
	return value
}

func mappingPair(node *yaml.Node, key string) (*yaml.Node, *yaml.Node) {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil, nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i], node.Content[i+1]
		}
	}
	return nil, nil
}

func scalarBlockEnd(lines []string, start, indent int) int {
	end := start + 1
	for end < len(lines) {
		line := strings.TrimSuffix(strings.TrimSuffix(lines[end], "\n"), "\r")
		if strings.TrimSpace(line) != "" {
			currentIndent := len(line) - len(strings.TrimLeft(line, " "))
			if currentIndent <= indent {
				break
			}
		}
		end++
	}
	return end
}

func renderScalarContent(code string, indent int) string {
	lines := strings.Split(strings.TrimSuffix(code, "\n"), "\n")
	var out strings.Builder
	for _, line := range lines {
		if line == "" {
			out.WriteByte('\n')
		} else {
			out.WriteString(strings.Repeat(" ", indent) + line + "\n")
		}
	}
	return out.String()
}

func sequenceItemIndent(lines []string, line int) int {
	if line < 0 || line >= len(lines) {
		return 0
	}
	text := strings.TrimLeft(lines[line], " ")
	return len(lines[line]) - len(text)
}

func entryEndLine(lines []string, entryLine, indent int) int {
	for lineIndex := entryLine + 1; lineIndex < len(lines); lineIndex++ {
		line := strings.TrimSuffix(strings.TrimSuffix(lines[lineIndex], "\n"), "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		lineIndent := len(line) - len(strings.TrimLeft(line, " "))
		if lineIndent <= indent {
			return lineIndex
		}
	}
	return len(lines)
}

func writeUnifiedDiff(w io.Writer, old, generated string) error {
	diff, err := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
		A: difflib.SplitLines(old), B: difflib.SplitLines(generated),
		FromFile: "manifest.yaml", ToFile: "generated manifest.yaml", Context: 3,
	})
	if err != nil {
		return err
	}
	_, err = io.WriteString(w, diff)
	return err
}
