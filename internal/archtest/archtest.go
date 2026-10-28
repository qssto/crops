// Package archtest 用 go/parser 静态检查分层 import 规则与 time.Now 使用位置（母文档 L1 任务 11/12）。
// 只扫描仓库根下 internal/ 与 cmd/ 的非 _test.go 文件。
package archtest

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Module 是 go.mod 中的模块路径。
const Module = "agri-steward"

// File 是一个被扫描的 Go 源文件。
type File struct {
	Rel     string   // 相对仓库根的路径，如 internal/clock/system.go
	Pkg     string   // 相对仓库根的目录，如 internal/clock
	Imports []string // import 路径
	NowAt   []int    // 出现 time.Now 选择子的行号
}

// Violation 是一条违规。
type Violation struct {
	File string
	Line int
	Rule string
}

func (v Violation) String() string {
	if v.Line > 0 {
		return fmt.Sprintf("%s:%d: %s", v.File, v.Line, v.Rule)
	}
	return v.File + ": " + v.Rule
}

// Scan 解析 root 下 internal/ 与 cmd/ 的非测试 Go 文件。
func Scan(root string) ([]File, error) {
	var files []File
	fset := token.NewFileSet()
	for _, top := range []string{"internal", "cmd"} {
		dir := filepath.Join(root, top)
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, err := parseFile(fset, root, path)
			if err != nil {
				return err
			}
			files = append(files, f)
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("archtest: scan %s: %w", dir, err)
		}
	}
	return files, nil
}

func parseFile(fset *token.FileSet, root, path string) (File, error) {
	af, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		return File{}, fmt.Errorf("parse %s: %w", path, err)
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return File{}, fmt.Errorf("rel %s: %w", path, err)
	}
	rel = filepath.ToSlash(rel)
	f := File{Rel: rel, Pkg: filepath.ToSlash(filepath.Dir(rel))}
	timeName := ""
	for _, imp := range af.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			return File{}, fmt.Errorf("%s: import %s: %w", rel, imp.Path.Value, err)
		}
		f.Imports = append(f.Imports, p)
		if p == "time" {
			timeName = "time"
			if imp.Name != nil {
				timeName = imp.Name.Name
			}
		}
	}
	if timeName != "" {
		ast.Inspect(af, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Now" {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == timeName {
				f.NowAt = append(f.NowAt, fset.Position(sel.Pos()).Line)
			}
			return true
		})
	}
	return f, nil
}

// Check 对文件逐条套用 Rules，返回按文件、行号排序的违规。
func Check(files []File) []Violation {
	var out []Violation
	for _, f := range files {
		for _, r := range Rules {
			out = append(out, r.check(f)...)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		return out[i].Line < out[j].Line
	})
	return out
}
