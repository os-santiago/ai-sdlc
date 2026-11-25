package podtransport

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The sandbox RuntimeClass is read only before a claim (each PollOnce) and by
// the readiness preflights. No in-flight path reads it again: a running
// executor, verification or review uses the admission observed before its
// claim, so a transient read failure can never stop work in progress. A new
// caller must keep that property and be added here deliberately.
func TestRuntimeClassIsReadOnlyBeforeClaimsAndForReadiness(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	var callers []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "CheckRuntimeClass" {
						callers = append(callers, filepath.ToSlash(rel)+":"+fn.Name.Name)
					}
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(callers)
	want := []string{
		"components/verifier/cmd/main.go:main", // readiness preflight
		"internal/execution/runtime.go:CheckSandbox",
		"internal/execution/runtime.go:PollOnce",
		"internal/reviewflow/runtime.go:PollOnce",
		"internal/verificationflow/runtime.go:PollOnce",
	}
	if strings.Join(callers, ",") != strings.Join(want, ",") {
		t.Fatalf("RuntimeClass readers %q, want %q", callers, want)
	}
}
