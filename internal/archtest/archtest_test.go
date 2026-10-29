package archtest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const repoRoot = "../.."

// TestRepo 是 make test 中的分层守卫：仓库当前代码必须零违规。
func TestRepo(t *testing.T) {
	files, err := Scan(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("scanned 0 files; wrong root?")
	}
	for _, v := range Check(files) {
		t.Errorf("violation: %s", v)
	}
}

// TestDetectsViolations 在临时目录种下违规，证明每条规则确实能拦住。
func TestDetectsViolations(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		src      string
		wantRule string // 空表示应零违规
	}{
		{name: "agent imports service", path: "internal/agent/x.go",
			src: `import _ "agri-steward/internal/service"`, wantRule: "agent must not import"},
		{name: "harness imports knowledge", path: "internal/agent/harness/x.go",
			src: `import _ "agri-steward/internal/agent/knowledge"`, wantRule: "agent/harness must not import"},
		{name: "service imports evalkit", path: "internal/service/x.go",
			src: `import _ "agri-steward/internal/evalkit"`, wantRule: "must not import evalkit"},
		{name: "cmd/eval may import evalkit", path: "cmd/eval/x.go",
			src: `import _ "agri-steward/internal/evalkit"`},
		{name: "model imports third party", path: "internal/model/x.go",
			src: `import _ "gopkg.in/yaml.v3"`, wantRule: "model imports stdlib only"},
		{name: "model imports internal", path: "internal/model/x.go",
			src: `import _ "agri-steward/internal/clock"`, wantRule: "model imports stdlib only"},
		{name: "controller imports repository", path: "internal/controller/x.go",
			src: `import _ "agri-steward/internal/repository"`, wantRule: "controller may import only"},
		{name: "service imports gorm", path: "internal/service/x.go",
			src: `import _ "gorm.io/gorm"`, wantRule: "only repository/app/cmd may import gorm"},
		{name: "repository may import gorm", path: "internal/repository/x.go", src: `import _ "gorm.io/gorm"`},
		{name: "service imports viper", path: "internal/service/x.go",
			src: `import _ "github.com/spf13/viper"`, wantRule: "only config may import viper"},
		{name: "service imports gin", path: "internal/service/x.go",
			src: `import _ "github.com/gin-gonic/gin"`, wantRule: "only controller may import gin"},
		{name: "time.Now in service", path: "internal/service/x.go",
			src: "import \"time\"\nvar _ = time.Now()", wantRule: "time.Now only in"},
		{name: "aliased time.Now", path: "cmd/server/x.go",
			src: "import tm \"time\"\nvar f = tm.Now", wantRule: "time.Now only in"},
		{name: "time.Now in clock/system.go", path: "internal/clock/system.go",
			src: "import \"time\"\nvar _ = time.Now()"},
		{name: "service imports adapter", path: "internal/service/x.go",
			src: `import _ "agri-steward/internal/adapter"`, wantRule: "may import adapter"},
		{name: "dispatcher may import adapter", path: "internal/service/workorder_dispatch.go",
			src: `import _ "agri-steward/internal/adapter"`},
		{name: "agent imports adapter", path: "internal/agent/graph/x.go",
			src: `import _ "agri-steward/internal/adapter"`, wantRule: "may import adapter"},
		{name: "time.Since is fine", path: "internal/service/x.go",
			src: "import \"time\"\nvar _ = time.Duration(1)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			for _, d := range []string{"internal", "cmd"} {
				if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			p := filepath.Join(root, tt.path)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			src := "package " + filepath.Base(filepath.Dir(p)) + "\n" + tt.src + "\n"
			if err := os.WriteFile(p, []byte(src), 0o600); err != nil {
				t.Fatal(err)
			}
			files, err := Scan(root)
			if err != nil {
				t.Fatal(err)
			}
			vs := Check(files)
			if tt.wantRule == "" {
				if len(vs) != 0 {
					t.Fatalf("want no violation, got %v", vs)
				}
				return
			}
			if len(vs) != 1 || !strings.Contains(vs[0].Rule, tt.wantRule) {
				t.Fatalf("violations = %v, want one containing %q", vs, tt.wantRule)
			}
		})
	}
}

func TestScan_SkipsTestFilesAndBadRoot(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "internal", "service")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "cmd"), 0o755); err != nil {
		t.Fatal(err)
	}
	src := "package service\nimport \"time\"\nvar _ = time.Now()\n"
	if err := os.WriteFile(filepath.Join(dir, "x_test.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	files, err := Scan(root)
	if err != nil || len(files) != 0 {
		t.Fatalf("files=%v err=%v, want 0 files", files, err)
	}
	if _, err := Scan(filepath.Join(root, "missing")); err == nil {
		t.Fatal("want error for missing root")
	}
}
