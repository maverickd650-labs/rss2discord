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

package msgtemplate

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// sandboxDir is the only production directory allowed to import the template
// engines.
const sandboxDir = "internal/msgtemplate/"

// Paths of non-sandbox files the checker tests pretend to parse.
const (
	controllerFile = "internal/controller/x.go"
	otherFile      = "internal/rss/x.go"
)

// templateEngines are the packages that parse or run templates. Anything that
// imports them can build a template without Compile's construct checks or
// Execute's output cap.
var templateEngines = []string{"text/template", "text/template/parse", "html/template"}

// forbiddenImports returns the template-engine imports in src, a Go source
// file at rel (slash-separated, relative to the module root), or nil when rel
// is inside the sandbox.
func forbiddenImports(t *testing.T, rel, src string) []string {
	t.Helper()
	if strings.HasPrefix(rel, sandboxDir) {
		return nil
	}
	file, err := parser.ParseFile(token.NewFileSet(), rel, src, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	var problems []string
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			t.Fatal(err)
		}
		if slices.Contains(templateEngines, path) {
			problems = append(problems, rel+": imports "+path+"; message templates must go through internal/msgtemplate")
		}
	}
	return problems
}

// Only the sandbox may import a template engine, so every message template is
// compiled by Compile and run by Execute. Mirrors
// TestProductionCallersKeepTheSSRFGuard in internal/rss.
func TestOnlyTheSandboxImportsTextTemplate(t *testing.T) {
	root := moduleRoot(t)
	skipDirs := []string{".git", ".claude", "bin", "vendor", "testdata", "dist", "node_modules"}
	var problems []string
	files := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && slices.Contains(skipDirs, d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files++
		problems = append(problems, forbiddenImports(t, filepath.ToSlash(rel), string(src))...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files == 0 {
		t.Fatal("no production Go files found; the walk is broken")
	}
	for _, p := range problems {
		t.Error(p)
	}
}

// The checker must flag each template engine, however imported, outside the
// sandbox and accept the sandbox and unrelated imports.
func TestForbiddenImports(t *testing.T) {
	for _, tc := range []struct {
		name, rel, imports string
		want               int
	}{
		{name: "text/template", rel: controllerFile, imports: `"text/template"`, want: 1},
		{name: "parse", rel: controllerFile, imports: `"text/template/parse"`, want: 1},
		{name: "html/template", rel: "cmd/x.go", imports: `"html/template"`, want: 1},
		{name: "renamed", rel: otherFile, imports: `tt "text/template"`, want: 1},
		{name: "dot", rel: otherFile, imports: `. "text/template"`, want: 1},
		{name: "blank", rel: otherFile, imports: `_ "text/template"`, want: 1},
		{name: "sandbox itself", rel: sandboxDir + "x.go", imports: `"text/template"`},
		{name: "unrelated", rel: controllerFile, imports: `"strings"`},
		{name: "lookalike", rel: controllerFile, imports: `"example.com/text/template"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := "package p\n\nimport " + tc.imports + "\n"
			if got := forbiddenImports(t, tc.rel, src); len(got) != tc.want {
				t.Fatalf("got %d problems, want %d: %q", len(got), tc.want, got)
			}
		})
	}
}

// moduleRoot walks up from the test's working directory to the directory
// holding go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test directory")
		}
		dir = parent
	}
}
