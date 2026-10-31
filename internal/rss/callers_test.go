package rss

import (
	"fmt"
	"go/ast"
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

const (
	rssImportPath  = "github.com/maverickd650/rss2discord-operator/internal/rss"
	httpImportPath = "net/http"
)

// httpClientOwners are the only production files allowed to build an
// *http.Client or *http.Transport or to touch net/http's shared defaults: the
// SSRF-guarded feed transport, and the Discord client, which only dials the
// AllowedWebhookHosts allowlist.
var httpClientOwners = []string{"internal/rss/transport.go", "internal/discord/client.go"}

// transportWrapCallers are the only production directories allowed to build an
// RSS client with NewClientWithTransportWrap: the manager, which wraps the
// guarded transport for tracing.
var transportWrapCallers = []string{"cmd/"}

// checkHTTPClientUse reports every place in file (at rel, slash-separated and
// relative to the module root) where production code could fetch feeds without
// the SSRF guard: an rss.NewClient call given anything but a literal nil (its
// *http.Client argument is used unguarded), an rss.NewClientWithTransportWrap
// call outside transportWrapCallers, and, outside httpClientOwners, a
// net/http Client or Transport literal, http.DefaultClient,
// http.DefaultTransport or a package-level request helper (http.Get, ...).
func checkHTTPClientUse(fset *token.FileSet, file *ast.File, rel string) []string {
	imports := map[string]string{} // local name -> import path
	for _, spec := range file.Imports {
		path, _ := strconv.Unquote(spec.Path.Value)
		name := path[strings.LastIndex(path, "/")+1:]
		if spec.Name != nil {
			name = spec.Name.Name
		}
		imports[name] = path
	}
	pkgOf := func(e ast.Expr) (string, string) {
		sel, ok := e.(*ast.SelectorExpr)
		if !ok {
			return "", ""
		}
		x, ok := sel.X.(*ast.Ident)
		if !ok {
			return "", ""
		}
		return imports[x.Name], sel.Sel.Name
	}
	ownsClients := slices.Contains(httpClientOwners, rel)
	mayWrap := slices.ContainsFunc(transportWrapCallers, func(dir string) bool { return strings.HasPrefix(rel, dir) })

	var problems []string
	report := func(n ast.Node, format string, args ...any) {
		problems = append(problems, fmt.Sprintf("%s: %s", fset.Position(n.Pos()), fmt.Sprintf(format, args...)))
	}
	ast.Inspect(file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CallExpr:
			switch pkg, name := pkgOf(n.Fun); {
			case pkg == rssImportPath && name == "NewClient":
				if len(n.Args) != 1 || !isNilIdent(n.Args[0]) {
					report(n, "rss.NewClient must be called with nil so it uses the SSRF-guarded transport")
				}
			case pkg == rssImportPath && name == "NewClientWithTransportWrap" && !mayWrap:
				report(n, "rss.NewClientWithTransportWrap is reserved for %v", transportWrapCallers)
			case pkg == httpImportPath && !ownsClients && slices.Contains([]string{"Get", "Head", "Post", "PostForm"}, name):
				report(n, "http.%s uses http.DefaultClient, which bypasses the SSRF guard", name)
			}
		case *ast.CompositeLit:
			if pkg, name := pkgOf(n.Type); pkg == httpImportPath && !ownsClients && (name == "Client" || name == "Transport") {
				report(n, "http.%s literal outside %v; feed fetching must go through internal/rss/transport.go", name, httpClientOwners)
			}
		case *ast.SelectorExpr:
			if pkg, name := pkgOf(n); pkg == httpImportPath && !ownsClients && (name == "DefaultClient" || name == "DefaultTransport") {
				report(n, "http.%s outside %v bypasses the SSRF guard", name, httpClientOwners)
			}
		}
		return true
	})
	return problems
}

func isNilIdent(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == "nil"
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

// Every production caller in the module must get its feed client through the
// SSRF guard; see checkHTTPClientUse. This keeps a future custom *http.Client
// or transport in a caller from escaping the guard in internal/rss/transport.go.
func TestProductionCallersKeepTheSSRFGuard(t *testing.T) {
	root := moduleRoot(t)
	skipDirs := []string{".git", ".claude", "bin", "vendor", "testdata", "dist", "node_modules"}
	fset := token.NewFileSet()
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
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files++
		problems = append(problems, checkHTTPClientUse(fset, file, filepath.ToSlash(rel))...)
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

// The checker itself must flag each way around the guard and accept the
// sanctioned constructors.
func TestCheckHTTPClientUse(t *testing.T) {
	const header = `package p

import (
	stdhttp "net/http"
	feeds "` + rssImportPath + `"
)

`
	const caller = "internal/controller/x.go"
	for _, tc := range []struct {
		name, rel, body string
		wantProblems    int
	}{
		{name: "nil client", rel: caller, body: `var _ = feeds.NewClient(nil)`},
		{name: "custom client", rel: caller, body: `var _ = feeds.NewClient(&stdhttp.Client{})`, wantProblems: 2},
		{name: "client variable", rel: caller, body: `var c *stdhttp.Client; var _ = feeds.NewClient(c)`, wantProblems: 1},
		{name: "wrap in cmd", rel: "cmd/main.go", body: `var _ = feeds.NewClientWithTransportWrap(nil)`},
		{name: "wrap elsewhere", rel: caller, body: `var _ = feeds.NewClientWithTransportWrap(nil)`, wantProblems: 1},
		{name: "transport literal", rel: caller, body: `var _ = &stdhttp.Transport{}`, wantProblems: 1},
		{name: "default client", rel: caller, body: `var _ = stdhttp.DefaultClient`, wantProblems: 1},
		{name: "default transport", rel: caller, body: `var _ = stdhttp.DefaultTransport`, wantProblems: 1},
		{name: "package-level get", rel: caller, body: `var _, _ = stdhttp.Get("https://example.com/feed.xml")`, wantProblems: 1},
		{name: "owner may build clients", rel: "internal/rss/transport.go", body: `var _ = &stdhttp.Client{Transport: stdhttp.DefaultTransport}`},
		{name: "unrelated selector", rel: caller, body: `var _ = stdhttp.MethodGet`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, tc.rel, header+tc.body+"\n", parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			if got := checkHTTPClientUse(fset, file, tc.rel); len(got) != tc.wantProblems {
				t.Fatalf("got %d problems, want %d: %q", len(got), tc.wantProblems, got)
			}
		})
	}
}
