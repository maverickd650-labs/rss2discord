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
	"fmt"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeTB struct {
	testing.TB
	failed bool
	fatals []string
}

func (f *fakeTB) Fatal(args ...any) {
	f.failed = true
	f.fatals = append(f.fatals, fmt.Sprint(args...))
}

func (f *fakeTB) Helper() {}

func TestModuleRoot(t *testing.T) {
	root := ModuleRoot(t)
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("go.mod not found in %s: %v", root, err)
	}
}

func TestModuleRoot_NotFound(t *testing.T) {
	tmp := t.TempDir()
	t.Chdir(tmp)
	fake := &fakeTB{}
	_ = ModuleRoot(fake)
	if !fake.failed || len(fake.fatals) == 0 {
		t.Fatal("ModuleRoot in temp dir did not fail")
	}
}

func TestFindModuleRoot_NotFound(t *testing.T) {
	tmp := t.TempDir()
	if _, err := FindModuleRoot(tmp); err == nil {
		t.Fatal("FindModuleRoot on empty temp dir succeeded, want error")
	}
}

func TestProductionFiles(t *testing.T) {
	fset := token.NewFileSet()
	count := 0
	for rel, file := range ProductionFiles(t, fset) {
		count++
		if filepath.IsAbs(rel) {
			t.Fatalf("rel %q must not be absolute", rel)
		}
		if strings.Contains(rel, "\\") {
			t.Fatalf("rel %q must be slash-separated", rel)
		}
		if strings.HasSuffix(rel, "_test.go") {
			t.Fatalf("rel %q must not be a test file", rel)
		}
		if file == nil {
			t.Fatalf("file AST for %q is nil", rel)
		}
	}
	if count == 0 {
		t.Fatal("ProductionFiles yielded 0 files")
	}

	// Test early break
	broken := 0
	for range ProductionFiles(t, nil) {
		broken++
		break
	}
	if broken != 1 {
		t.Fatalf("expected 1 file yielded on early break, got %d", broken)
	}
}

func TestWalkProductionFiles_Filters(t *testing.T) {
	tmp := t.TempDir()
	// Create production file
	if err := os.WriteFile(filepath.Join(tmp, "prod.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Create test file
	if err := os.WriteFile(filepath.Join(tmp, "prod_test.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Create skipped dir file
	gitDir := filepath.Join(tmp, ".git")
	if err := os.Mkdir(gitDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "ignored.go"), []byte("package git\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Create non-go file
	if err := os.WriteFile(filepath.Join(tmp, "README.md"), []byte("# hi\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var seen []string
	for rel := range WalkProductionFiles(t, tmp, nil, 0) {
		seen = append(seen, rel)
	}
	if len(seen) != 1 || seen[0] != "prod.go" {
		t.Fatalf("seen = %v, want exactly [prod.go]", seen)
	}
}

func TestWalkProductionFiles_EmptyDir(t *testing.T) {
	tmp := t.TempDir()
	fake := &fakeTB{}
	for range WalkProductionFiles(fake, tmp, nil, 0) {
	}
	if !fake.failed || len(fake.fatals) == 0 {
		t.Fatal("WalkProductionFiles on empty dir did not fail")
	}
}

func TestWalkProductionFiles_ParseError(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "bad.go"), []byte("package bad\nfunc ( {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := &fakeTB{}
	for range WalkProductionFiles(fake, tmp, nil, 0) {
	}
	if !fake.failed || len(fake.fatals) == 0 {
		t.Fatal("WalkProductionFiles on bad Go file did not fail")
	}
}

func TestWalkProductionFiles_WalkError(t *testing.T) {
	fake := &fakeTB{}
	nonExistent := filepath.Join(t.TempDir(), "does-not-exist")
	for range WalkProductionFiles(fake, nonExistent, nil, 0) {
	}
	if !fake.failed || len(fake.fatals) == 0 {
		t.Fatal("WalkProductionFiles on non-existent root did not fail")
	}
}
