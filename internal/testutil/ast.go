/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package testutil

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"iter"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// DefaultSkipDirs are directory names skipped when walking a repository for
// production Go files.
var DefaultSkipDirs = []string{".git", ".claude", "bin", "vendor", "testdata", "dist", "node_modules"}

// FindModuleRoot walks up from startDir until it finds a directory containing go.mod.
func FindModuleRoot(startDir string) (string, error) {
	abs, _ := filepath.Abs(startDir)
	dir := abs
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("go.mod not found above " + startDir)
		}
		dir = parent
	}
}

// ModuleRoot walks up from the test's working directory to the directory
// holding go.mod.
func ModuleRoot(t testing.TB) string {
	t.Helper()
	root, err := FindModuleRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// WalkProductionFiles yields (relPath, *ast.File) for every non-test Go source file
// under root, skipping DefaultSkipDirs. relPath is slash-separated and relative to root.
// If fset is nil, a new token.FileSet is used.
func WalkProductionFiles(t testing.TB, root string, fset *token.FileSet, mode parser.Mode) iter.Seq2[string, *ast.File] {
	if fset == nil {
		fset = token.NewFileSet()
	}
	return func(yield func(string, *ast.File) bool) {
		t.Helper()
		files := 0
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if path != root && slices.Contains(DefaultSkipDirs, d.Name()) {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, err := parser.ParseFile(fset, path, nil, mode)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			files++
			if !yield(filepath.ToSlash(rel), file) {
				return fs.SkipAll
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if files == 0 {
			t.Fatal("no production Go files found; the walk is broken")
		}
	}
}

// ProductionFiles returns an iterator over non-test Go source files in the module root,
// parsed with parser.SkipObjectResolution.
func ProductionFiles(t testing.TB, fset *token.FileSet) iter.Seq2[string, *ast.File] {
	t.Helper()
	return WalkProductionFiles(t, ModuleRoot(t), fset, parser.SkipObjectResolution)
}
