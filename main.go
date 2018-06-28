// Command gofixunkeyedcomposites adds keys to composite literal fields.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/importer"
	"go/parser"
	"go/scanner"
	"go/token"
	"go/types"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
)

func main() {
	overwrite := flag.Bool("w", false, "write result to (source) file instead of stdout")
	list := flag.Bool("l", false, "list files whose formatting differs from gofixunkeyedcomposites's")
	flag.Usage = func() {
		fmt.Println(helpMsg)
		flag.PrintDefaults()
	}
	flag.Parse()
	paths := flag.Args()

	if len(paths) == 0 {
		if *overwrite {
			fmt.Fprintln(os.Stderr, "can't use -w on stdin")
			os.Exit(1)
		}
		var w io.Writer
		if !*list {
			w = os.Stdout
		}
		fixed, err := fixFile(w, os.Stdin, "")
		if err != nil {
			reportErrs(err)
			os.Exit(1)
		}
		if fixed && *list {
			fmt.Println("<standard input>")
		}
		return
	}

	for _, path := range paths {
		var w io.Writer
		var buf *bytes.Buffer
		if *overwrite {
			buf = bytes.NewBuffer(nil)
			w = buf
		} else if !*list {
			w = os.Stdout
		}

		absPath, err := filepath.Abs(path)
		if err != nil {
			reportErrs(err)
			os.Exit(1)
		}
		fixed, err := fixFile(w, nil, absPath)
		if err != nil {
			reportErrs(err)
			os.Exit(1)
		}

		if fixed && *list {
			fmt.Println(path)
		}
		if *overwrite {
			err := ioutil.WriteFile(path, buf.Bytes(), 0655)
			if err != nil {
				reportErrs(err)
				os.Exit(1)
			}
		}
	}
}

const helpMsg = `gofixunkeyedcomposites adds keys to composite literal fields.

Usage:

	gofixunkeyedcomposites [options] [path ...]

Options:
`

func reportErrs(errs ...error) {
	for _, err := range errs {
		if errs, ok := err.(scanner.ErrorList); ok {
			for _, err := range errs {
				fmt.Fprintln(os.Stderr, err)
			}
		} else {
			fmt.Fprintln(os.Stderr, err)
		}
	}
}

func fixFile(w io.Writer, r io.Reader, path string) (fixed bool, err error) {
	dir := "."
	if path != "" {
		dir = filepath.Dir(path)
	}

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, nil, parser.ParseComments)
	if err != nil {
		return false, err
	}

	var pkg *ast.Package
	var file *ast.File

	if path == "" {
		file, err = parser.ParseFile(fset, "stdin.go", r, 0)
		if err != nil {
			return false, err
		}
		var ok bool
		pkg, ok = findPkgForStdinFile(fset, pkgs, file)
		if !ok {
			pkg, err = ast.NewPackage(fset, map[string]*ast.File{
				"stdin.go": file,
			}, nil, nil)
			if err != nil {
				return false, err
			}
		}
	} else {
		var ok bool
		pkg, file, ok = findPkgForFile(fset, pkgs, path)
		if !ok {
			return false, fmt.Errorf("%s: not a Go file within a package", path)
		}
	}

	cfg := &types.Config{
		Error: func(error) {
			// Just ignore typing errors; not our concern.
		},
		Importer:                 importer.For("source", nil).(types.ImporterFrom),
		DisableUnusedImportCheck: true,
	}
	info := &types.Info{
		Types: map[ast.Expr]types.TypeAndValue{},
	}
	astFiles := make([]*ast.File, 0, len(pkg.Files))
	for _, f := range pkg.Files {
		astFiles = append(astFiles, f)
	}

	cwd, err := os.Getwd()
	if err != nil {
		return false, err
	}
	cfg.Check(cwd, fset, astFiles, info)

	v := &visitor{file: fset.File(file.Pos()), types: info.Types}
	if w != nil {
		v.in, err = ioutil.ReadFile(path)
		if err != nil {
			return false, err
		}
	}

	ast.Walk(v, file)

	if w != nil {
		out, err := format.Source(v.out())
		if err != nil {
			return false, err
		}

		_, err = io.Copy(w, bytes.NewReader(out))
		if err != nil {
			return false, err
		}

	}

	return v.fixed, err
}

type chunk struct {
	offset int
	b      []byte
}

type visitor struct {
	file  *token.File
	types map[ast.Expr]types.TypeAndValue
	in    []byte

	added []chunk

	fixed bool
}

func (v *visitor) out() []byte {
	sort.Slice(v.added, func(i, j int) bool {
		return v.added[i].offset < v.added[j].offset
	})

	var out []byte
	var offset int
	for _, chunk := range v.added {
		out = append(out, v.in[offset:chunk.offset]...)
		out = append(out, chunk.b...)
		offset = chunk.offset
	}
	out = append(out, v.in[offset:]...)

	return out
}

func (v *visitor) writeAfter(pos token.Pos, s string) {
	v.added = append(v.added, chunk{offset: v.file.Offset(pos), b: []byte(s)})
}

func (v *visitor) Visit(node ast.Node) ast.Visitor {
	lit, ok := node.(*ast.CompositeLit)
	if !ok {
		return v
	}

	typ, ok := v.types[lit]
	if !ok {
		return v
	}
	s, ok := assertStructType(typ.Type)
	if !ok {
		return v
	}

	if s.NumFields() == 0 {
		// Empty struct; no keys to add.
		return v
	}
	if len(lit.Elts) != s.NumFields() {
		// Either already has keys or missing fields; nothing to add.
		return v
	}
	if len(lit.Elts) > 0 {
		if _, ok := lit.Elts[0].(*ast.KeyValueExpr); ok {
			// Already has keys; nothing to add.
			return v
		}
	}

	if v.in != nil {
		for i := 0; i < s.NumFields(); i++ {
			v.writeAfter(lit.Elts[i].Pos(), s.Field(i).Name()+": ")
		}
	}

	v.fixed = true

	return v
}

func assertStructType(typ types.Type) (*types.Struct, bool) {
	if p, ok := typ.(*types.Pointer); ok {
		typ = p.Elem()
	}
	if n, ok := typ.(*types.Named); ok {
		typ = n.Underlying()
	}
	s, ok := typ.(*types.Struct)
	return s, ok
}

func findPkgForFile(fset *token.FileSet, pkgs map[string]*ast.Package, path string) (*ast.Package, *ast.File, bool) {
	for _, pkg := range pkgs {
		for fileName, file := range pkg.Files {
			if fileName == filepath.Clean(path) {
				return pkg, file, true
			}
		}
	}

	return nil, nil, false
}

func findPkgForStdinFile(fset *token.FileSet, pkgs map[string]*ast.Package, stdinFile *ast.File) (*ast.Package, bool) {
	for pkgName, pkg := range pkgs {
		if pkgName == stdinFile.Name.Name {
			return pkg, true
		}
	}
	return nil, false
}
