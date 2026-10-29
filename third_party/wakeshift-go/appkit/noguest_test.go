package appkit

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"testing"
)

// Guest sign-in left the SDK: apps identify their own users with their own
// sign-in. Nothing the package exports offers it.
func TestNoGuestSignIn(t *testing.T) {
	pkgs, err := parser.ParseDir(token.NewFileSet(), ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pkgs {
		for name, f := range p.Files {
			for _, d := range f.Decls {
				var names []string
				switch d := d.(type) {
				case *ast.FuncDecl:
					names = append(names, d.Name.Name)
				case *ast.GenDecl:
					for _, s := range d.Specs {
						switch s := s.(type) {
						case *ast.TypeSpec:
							names = append(names, s.Name.Name)
						case *ast.ValueSpec:
							for _, n := range s.Names {
								names = append(names, n.Name)
							}
						}
					}
				}
				for _, n := range names {
					if ast.IsExported(n) && strings.Contains(strings.ToLower(n), "guest") {
						t.Errorf("%s exports %s", name, n)
					}
				}
			}
		}
	}
}
