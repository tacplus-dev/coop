// Copyright (c) 2026 Jody Scott
// SPDX-License-Identifier: Apache-2.0

package bootstrap

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bootstrap.yaml")
	if err := os.WriteFile(path, []byte("kernel: k\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Read(path)
	if err != nil {
		t.Fatalf("Read(%q): %v", path, err)
	}
	if string(got) != "kernel: k\n" {
		t.Fatalf("Read(%q) = %q", path, got)
	}
	// An unclean path to the same file is read the same.
	if _, err := Read(dir + "/./sub/../bootstrap.yaml"); err != nil {
		t.Fatalf("Read of an unclean path: %v", err)
	}
}

// A ConfigMap or Secret volume mounts each key as a symlink through ..data
// to a timestamped directory, all inside the mount: it must still read.
func TestReadFileThroughVolumeSymlinks(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "..2026_11_14"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "..2026_11_14", "bootstrap.yaml"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("..2026_11_14", filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("..data/bootstrap.yaml", filepath.Join(dir, "bootstrap.yaml")); err != nil {
		t.Fatal(err)
	}
	if got, err := Read(filepath.Join(dir, "bootstrap.yaml")); err != nil || string(got) != "x" {
		t.Fatalf("Read through volume symlinks = %q, %v", got, err)
	}
}

func TestReadRefuses(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("s"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "escape.yaml")); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{
		"empty":                  "",
		"relative":               "bootstrap.yaml",
		"relative climbing":      "../bootstrap.yaml",
		"a directory":            dir,
		"missing":                filepath.Join(dir, "missing.yaml"),
		"symlink out of its dir": filepath.Join(dir, "escape.yaml"),
	} {
		if got, err := Read(path); err == nil {
			t.Errorf("%s: Read(%q) = %q, want an error", name, path, got)
		}
	}
}
