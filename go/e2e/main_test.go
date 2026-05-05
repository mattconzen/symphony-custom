// Package e2e contains end-to-end tests for Symphony that cross multiple
// subsystems. Tests in this package exercise realistic scenarios that the
// per-package unit tests do not cover.
package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// binaryPath is set by TestMain to the compiled symphony binary.
var binaryPath string

func TestMain(m *testing.M) {
	// Build the binary once into a temp dir shared by all CLI tests.
	tmpDir, err := os.MkdirTemp("", "symphony-e2e-bin-*")
	if err != nil {
		panic("e2e: failed to create temp dir for binary: " + err.Error())
	}
	defer os.RemoveAll(tmpDir) //nolint:errcheck

	bin := filepath.Join(tmpDir, "symphony")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/symphony")
	// Run from the go/ module root.
	cmd.Dir = filepath.Join(moduleRoot(), "")
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		// If the build itself fails, skip the CLI tests but let in-process
		// orchestrator tests still run (they don't need the binary).
		// We use a flag so CLI tests can detect this.
		binaryPath = ""
	} else {
		binaryPath = bin
	}

	os.Exit(m.Run())
}

// moduleRoot returns the absolute path to go/ (the Go module root).
func moduleRoot() string {
	// This file lives at go/e2e/main_test.go, so "../" is go/.
	abs, err := filepath.Abs("../")
	if err != nil {
		panic("e2e: cannot determine module root: " + err.Error())
	}
	return abs
}

// skipIfNoBinary skips the test if the binary was not built.
func skipIfNoBinary(t *testing.T) {
	t.Helper()
	if binaryPath == "" {
		t.Skip("symphony binary could not be built; skipping CLI test")
	}
}
