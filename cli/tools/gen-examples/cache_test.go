// Copyright Built On Envoy
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

// These tests verify that example runs reuse Envoy without sharing extension binaries or modifying the installed cache.
package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExampleEnvoyCacheReusesInstalledBinary(t *testing.T) {
	root, installed, runData := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("BOE_DATA_HOME", installed)
	source := filepath.Join(installed, "envoy-versions", "1.38.0", "bin", "envoy")
	require.NoError(t, os.MkdirAll(filepath.Dir(source), 0o700))
	require.NoError(t, os.WriteFile(source, []byte("installed binary"), 0o600))
	require.NoError(t, prepareEnvoyCache(root, runData, "1.38.0"))
	// #nosec G304 -- The path belongs to this test's temporary cache.
	contents, err := os.ReadFile(filepath.Join(runData, "envoy-versions", "1.38.0", "bin", "envoy"))
	require.NoError(t, err)
	require.Equal(t, "installed binary", string(contents))
	_, err = os.Stat(filepath.Join(runData, "extensions"))
	require.ErrorIs(t, err, os.ErrNotExist)
	// #nosec G304 -- The path belongs to this test's temporary installed cache.
	contents, err = os.ReadFile(source)
	require.NoError(t, err)
	require.Equal(t, "installed binary", string(contents))
	info, err := os.Stat(source)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	// A later run reuses the tool cache even if the original install is removed.
	require.NoError(t, os.Remove(source))
	require.NoError(t, prepareEnvoyCache(root, t.TempDir(), "1.38.0"))
}

func TestExampleEnvoyCacheLeavesMissingAndDevVersionsForInstaller(t *testing.T) {
	for _, version := range []string{"1.38.0", "dev-latest"} {
		t.Run(version, func(t *testing.T) {
			root, runData := t.TempDir(), t.TempDir()
			t.Setenv("BOE_DATA_HOME", t.TempDir())
			require.NoError(t, prepareEnvoyCache(root, runData, version))
			_, err := os.Stat(filepath.Join(runData, "envoy-versions", version, "bin", "envoy"))
			require.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}
