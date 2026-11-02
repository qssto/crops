package repository

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// sqlScopeExempt 是 WHERE 不以 tenant_id=? AND farm_id=? 开头的非扫描方法（扫描方法以 SystemScope 入参自动识别）；
// 键一律为「接收者.方法」（不按裸函数名豁免，避免同名方法被一并豁免）。
var sqlScopeExempt = map[string]string{
	"DecisionRepo.DecisionByRequest": "uk_request 全局唯一，调用方比对返回行归属（P0 定版）",
	"PartitionRepo.Partitions":       "information_schema 分区运维，非业务表",
	// P1-L0：开关按 GLOBAL / TENANT / FARM 三层存放，读取要合并上层行，写与列表只给 platform_admin（service 层校验）。
	"FlagRepo.Rows":        "三层合并读取：(0,0) 与 (t,0) 行 + 本农场行",
	"FlagRepo.List":        "platform_admin 的全量开关列表",
	"FlagRepo.ScopeExists": "校验写入目标 (t,0) / (t,f) 存在",
	// P1-L1-A：身份解析在 Scope 确定之前执行。
	"UserRoleRepo.ListRoles":   "本农场绑定 + 平台位 (0,0) 绑定",
	"UserRoleRepo.FarmTenants": "由路径 farm_id 反查真实租户，供与 token tenant 比对",
}

var (
	whereClause = regexp.MustCompile(`(?is)\bWHERE\s+(.{0,80})`)
	scopePrefix = regexp.MustCompile(`^(\w+\.)?tenant_id\s*=\s*\?\s+AND\s+(\w+\.)?farm_id\s*=\s*\?`)
)

// repoFuncs 解析本包非测试文件，对每个有函数体的 FuncDecl 回调。
func repoFuncs(t *testing.T, fn func(fset *token.FileSet, fd *ast.FuncDecl)) {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		af, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range af.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil {
				fn(fset, fd)
			}
		}
	}
}

// TestSQL_ScopePrefix：§6.2 功能任务 2「所有 SQL 以 tenant_id=? AND farm_id=? 开头」的静态检查。
// 自由函数不豁免：SQL 须写在带 Scope 闸门的方法里，或由该方法传入完整 SQL。
func TestSQL_ScopePrefix(t *testing.T) {
	checked := 0
	repoFuncs(t, func(fset *token.FileSet, fd *ast.FuncDecl) {
		if _, ok := sqlScopeExempt[methodKey(fd)]; ok || takesSystemScope(fd) {
			return
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			bl, ok := n.(*ast.BasicLit)
			if !ok || bl.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(bl.Value)
			if err != nil {
				t.Fatalf("%s: unquote: %v", fset.Position(bl.Pos()), err)
			}
			for _, m := range whereClause.FindAllStringSubmatch(s, -1) {
				checked++
				if !scopePrefix.MatchString(m[1]) {
					t.Errorf("%s %s: WHERE %q does not start with tenant_id=? AND farm_id=?",
						fset.Position(bl.Pos()), methodKey(fd), m[1])
				}
			}
			return true
		})
	})
	if checked < 20 {
		t.Fatalf("only %d WHERE clauses checked; scanner broken?", checked)
	}
	for key := range sqlScopeExempt {
		if !strings.Contains(key, ".") {
			t.Errorf("exemption %q must be Receiver.Method", key)
		}
	}
}

// gormBuilders 是会在字符串字面量之外拼 WHERE / JOIN 的 GORM 链式方法：TestSQL_ScopePrefix 只扫字面量，
// 用这些方法写的条件会绕过前缀检查，故仓储非测试代码禁用（一律写原生 SQL）。
var gormBuilders = []string{"Where", "Joins", "Or", "Not"}

// TestSQL_NoGormBuilders：仓储非测试代码不得调用 .Where( / .Joins( 等链式条件方法。
func TestSQL_NoGormBuilders(t *testing.T) {
	repoFuncs(t, func(fset *token.FileSet, fd *ast.FuncDecl) {
		for _, pos := range builderCalls(fd.Body) {
			t.Errorf("%s %s: GORM builder bypasses the tenant_id/farm_id prefix check; use raw SQL", fset.Position(pos), methodKey(fd))
		}
	})
}

// builderCalls 返回 n 内 x.Where(...) / x.Joins(...) 等调用的位置。
func builderCalls(n ast.Node) []token.Pos {
	var out []token.Pos
	ast.Inspect(n, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && slices.Contains(gormBuilders, sel.Sel.Name) {
			out = append(out, sel.Sel.Pos())
		}
		return true
	})
	return out
}

func TestBuilderCalls(t *testing.T) {
	tests := []struct {
		src  string
		want int
	}{
		{`func f() { db.Raw("SELECT 1").Scan(&x) }`, 0},
		{`func f() { db.Where("id=?", 1).Find(&x) }`, 1},
		{`func f() { db.Joins("JOIN t").Where("a").Find(&x) }`, 2},
	}
	for _, tt := range tests {
		f, err := parser.ParseFile(token.NewFileSet(), "x.go", "package p\n"+tt.src, 0)
		if err != nil {
			t.Fatal(err)
		}
		if got := len(builderCalls(f)); got != tt.want {
			t.Errorf("%s: %d builder calls, want %d", tt.src, got, tt.want)
		}
	}
}

// methodKey 返回「接收者.方法」；自由函数返回函数名。
func methodKey(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	t := fd.Recv.List[0].Type
	if st, ok := t.(*ast.StarExpr); ok {
		t = st.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name + "." + fd.Name.Name
	}
	return fd.Name.Name
}

func takesSystemScope(fd *ast.FuncDecl) bool {
	for _, p := range fd.Type.Params.List {
		if id, ok := p.Type.(*ast.Ident); ok && id.Name == "SystemScope" {
			return true
		}
	}
	return false
}
