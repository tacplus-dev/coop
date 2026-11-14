// Copyright (c) 2026 Jody Scott
// SPDX-License-Identifier: Apache-2.0

// Package bootstrap reads the bootstrap config the operator names in the
// environment (BOOTSTRAP_CONFIG), confined to the directory it names.
package bootstrap

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Read reads the file at path, which must be absolute. It is cleaned and
// read through an os.Root on its directory, so neither the path nor a
// symlink in it reaches outside that directory.
func Read(path string) ([]byte, error) {
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) {
		return nil, fmt.Errorf("bootstrap config %q: not an absolute path", path)
	}
	root, err := os.OpenRoot(filepath.Dir(clean))
	if err != nil {
		return nil, fmt.Errorf("bootstrap config %q: %w", path, err)
	}
	defer root.Close()
	data, err := fs.ReadFile(root.FS(), filepath.Base(clean))
	if err != nil {
		return nil, fmt.Errorf("bootstrap config %q: %w", path, err)
	}
	return data, nil
}
