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
	rssPackage     = "rss"
	rssDir         = "internal/rss/"
	guardedFile    = rssDir + "transport.go"
)

// httpClientOwners are the only production files allowed to build an
// *http.Client or *http.Transport or to touch net/http's shared defaults: the
// SSRF-guarded feed transport, and the Discord client, which only dials the
// AllowedWebhookHosts allowlist.
var httpClientOwners = []string{guardedFile, "internal/discord/client.go"}

// transportWrapCallers are the only production directories allowed to build an
// RSS client with NewClientWithTransportWrap: the manager, which wraps the
// guarded transport for tracing.
var transportWrapCallers = []string{"cmd/"}

// defaultClientHelpers are net/http's package-level request functions, which
// all send through http.DefaultClient.
var defaultClientHelpers = []string{"Get", "Head", "Post", "PostForm"}

// clientUseChecker walks one production file for ways around the SSRF guard;
// see checkHTTPClientUse.
type clientUseChecker struct {
	fset        *token.FileSet
	imports     map[string]string // local name -> import path
	inRSS       bool              // file is in package rss, whose constructors are called unqualified
	ownsClients bool              // file is one of httpClientOwners
	mayWrap     bool              // file is under transportWrapCallers
	problems    []string
}

// checkHTTPClientUse reports every place in file (at rel, slash-separated and
// relative to the module root) where production code could fetch feeds without
// the SSRF guard: an rss.NewClient call (or, inside package rss, an
// unqualified NewClient call) given anything but a literal nil (its
// *http.Client argument is used unguarded), an rss.NewClientWithTransportWrap
// call outside transportWrapCallers, and, outside httpClientOwners, a
// net/http Client or Transport built by literal, new() or a zero-value var,
// http.DefaultClient, http.DefaultTransport or a package-level request helper
// (http.Get, ...).
func checkHTTPClientUse(fset *token.FileSet, file *ast.File, rel string) []string {
	c := &clientUseChecker{
		fset:        fset,
		imports:     map[string]string{},
		inRSS:       strings.HasPrefix(rel, rssDir) && file.Name.Name == rssPackage,
		ownsClients: slices.Contains(httpClientOwners, rel),
		mayWrap:     slices.ContainsFunc(transportWrapCallers, func(dir string) bool { return strings.HasPrefix(rel, dir) }),
	}
	for _, spec := range file.Imports {
		path, _ := strconv.Unquote(spec.Path.Value)
		name := path[strings.LastIndex(path, "/")+1:]
		if spec.Name != nil {
			name = spec.Name.Name
		}
		// A dot import makes the guarded identifiers unqualified, where
		// qualified cannot see them.
		if name == "." && (path == httpImportPath || path == rssImportPath) {
			c.report(spec, "dot import of %s hides its clients from the SSRF guard check", path)
		}
		c.imports[name] = path
	}
	ast.Inspect(file, c.visit)
	return c.problems
}

func (c *clientUseChecker) report(n ast.Node, format string, args ...any) {
	c.problems = append(c.problems, fmt.Sprintf("%s: %s", c.fset.Position(n.Pos()), fmt.Sprintf(format, args...)))
}

// qualified returns the import path and name of a pkg.Name selector.
func (c *clientUseChecker) qualified(e ast.Expr) (path, name string) {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return "", ""
	}
	x, ok := sel.X.(*ast.Ident)
	if !ok {
		return "", ""
	}
	return c.imports[x.Name], sel.Sel.Name
}

// clientType reports whether e names net/http's Client or Transport type.
func (c *clientUseChecker) clientType(e ast.Expr) (string, bool) {
	path, name := c.qualified(e)
	return name, path == httpImportPath && (name == "Client" || name == "Transport")
}

// rssConstructor returns the name of the package rss function fun calls,
// whether qualified or, inside package rss, unqualified.
func (c *clientUseChecker) rssConstructor(fun ast.Expr) string {
	if path, name := c.qualified(fun); path == rssImportPath {
		return name
	}
	if id, ok := fun.(*ast.Ident); ok && c.inRSS {
		return id.Name
	}
	return ""
}

func (c *clientUseChecker) visit(n ast.Node) bool {
	switch n := n.(type) {
	case *ast.CallExpr:
		c.checkCall(n)
	case *ast.CompositeLit:
		c.checkClientType(n, n.Type, "literal")
	case *ast.ValueSpec:
		// A zero-value http.Client (var c http.Client) is a usable, unguarded
		// client too.
		c.checkClientType(n, n.Type, "value")
	case *ast.SelectorExpr:
		if path, name := c.qualified(n); path == httpImportPath && !c.ownsClients && (name == "DefaultClient" || name == "DefaultTransport") {
			c.report(n, "http.%s outside %v bypasses the SSRF guard", name, httpClientOwners)
		}
	}
	return true
}

func (c *clientUseChecker) checkClientType(n ast.Node, typ ast.Expr, how string) {
	if name, ok := c.clientType(typ); ok && !c.ownsClients {
		c.report(n, "http.%s %s outside %v; feed fetching must go through %s", name, how, httpClientOwners, guardedFile)
	}
}

func (c *clientUseChecker) checkCall(n *ast.CallExpr) {
	if id, ok := n.Fun.(*ast.Ident); ok && id.Name == "new" && len(n.Args) == 1 {
		c.checkClientType(n, n.Args[0], "from new()")
	}
	switch c.rssConstructor(n.Fun) {
	case "NewClient":
		if len(n.Args) != 1 || !isNilIdent(n.Args[0]) {
			c.report(n, "rss.NewClient must be called with nil so it uses the SSRF-guarded transport")
		}
	case "NewClientWithTransportWrap":
		if !c.mayWrap {
			c.report(n, "rss.NewClientWithTransportWrap is reserved for %v", transportWrapCallers)
		}
	}
	if path, name := c.qualified(n.Fun); path == httpImportPath && !c.ownsClients && slices.Contains(defaultClientHelpers, name) {
		c.report(n, "http.%s uses http.DefaultClient, which bypasses the SSRF guard", name)
	}
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
	// extra imports
)

`
	const caller = "internal/controller/x.go"
	for _, tc := range []struct {
		name, rel, pkg, body string
		extraImport          string // spliced into the import block
		wantProblems         int
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
		{name: "owner may build clients", rel: guardedFile, body: `var _ = &stdhttp.Client{Transport: stdhttp.DefaultTransport}`},
		{name: "unrelated selector", rel: caller, body: `var _ = stdhttp.MethodGet`},
		{name: "new client", rel: caller, body: `var _ = new(stdhttp.Client)`, wantProblems: 1},
		{name: "new transport", rel: caller, body: `var _ = new(stdhttp.Transport)`, wantProblems: 1},
		{name: "zero-value client", rel: caller, body: `var c stdhttp.Client; var _ = c`, wantProblems: 1},
		{name: "client pointer type is fine", rel: caller, body: `var c *stdhttp.Client; var _ = c`},
		{name: "owner may new a client", rel: guardedFile, body: `var _ = new(stdhttp.Client)`},
		{name: "package-local custom client", rel: "internal/rss/client.go", pkg: rssPackage, body: `func NewClient(*stdhttp.Client) int { return 0 }; var _ = NewClient(new(stdhttp.Client))`, wantProblems: 2},
		{name: "package-local nil client", rel: "internal/rss/client.go", pkg: rssPackage, body: `func NewClient(*stdhttp.Client) int { return 0 }; var _ = NewClient(nil)`},
		{name: "package-local wrap", rel: "internal/rss/parse.go", pkg: rssPackage, body: `func NewClientWithTransportWrap(any) int { return 0 }; var _ = NewClientWithTransportWrap(nil)`, wantProblems: 1},
		{name: "dot import of net/http", rel: caller, extraImport: `. "net/http"`, body: `var _ = Client{}`, wantProblems: 1},
		{name: "dot import of rss", rel: caller, extraImport: `. "` + rssImportPath + `"`, body: `var _ = NewClient(nil)`, wantProblems: 1},
		{name: "dot import of another package", rel: caller, extraImport: `. "strings"`, body: `var _ = ToUpper("x")`},
		{name: "same name in another package", rel: caller, body: `func NewClient(*stdhttp.Client) int { return 0 }; var _ = NewClient(nil)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			src := strings.Replace(header, "// extra imports", tc.extraImport, 1) + tc.body + "\n"
			if tc.pkg != "" {
				src = strings.Replace(src, "package p", "package "+tc.pkg, 1)
			}
			file, err := parser.ParseFile(fset, tc.rel, src, parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			if got := checkHTTPClientUse(fset, file, tc.rel); len(got) != tc.wantProblems {
				t.Fatalf("got %d problems, want %d: %q", len(got), tc.wantProblems, got)
			}
		})
	}
}
