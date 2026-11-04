package sample

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFile 写测试文件（自动建目录）。
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// holdoutTree 建 suites/regression/pub.yaml 与 suites/holdout/protect/ 下一条可解析、一条不可解析的私有样本。
func holdoutTree(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "suites")
	writeFile(t, filepath.Join(root, "regression", "pub.yaml"), "id: rice-pub-0001\nsuite: regression\n")
	writeFile(t, filepath.Join(root, HoldoutDir, "protect", "rice-ho-prot-9001.yaml"), "id: rice-ho-prot-9001\nsuite: holdout\n")
	writeFile(t, filepath.Join(root, HoldoutDir, "protect", "rice-ho-prot-9002.yaml"), "{{not yaml 13800138000")
	return root
}

func writeReaders(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "holdout_readers.yaml")
	writeFile(t, p, content)
	return p
}

const readersYAML = "readers:\n  - id: qa-lead\n    role: owner\n  - id: ci-eval\n    role: ci\n"

// L1 评审 P1：LoadDir 默认不得走进 holdout/（不解析、不报其内容）。
func TestLoadDir_SkipsHoldout(t *testing.T) {
	root := holdoutTree(t)
	samples, problems, err := LoadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 1 || samples[0].ID != "rice-pub-0001" {
		t.Fatalf("samples = %d, want only the public one", len(samples))
	}
	for _, p := range problems {
		t.Errorf("holdout content was parsed: %s", p)
	}
	// 直接把 holdout 目录当 root：无授权拒绝，不读任何文件。
	samples, problems, err = LoadDir(filepath.Join(root, HoldoutDir))
	if !errors.Is(err, ErrHoldoutDenied) || len(samples) != 0 || len(problems) != 0 {
		t.Fatalf("LoadDir(holdout) = %d samples, %d problems, %v; want ErrHoldoutDenied", len(samples), len(problems), err)
	}
}

// L2 评审 P1-1：holdout 按真实目录判定，换个根目录、换大小写、走符号链接都绕不过去。
func TestLoad_HoldoutBypasses(t *testing.T) {
	repo := t.TempDir()
	suites := filepath.Join(repo, "eval", "suites")
	writeFile(t, filepath.Join(suites, "regression", "pub.yaml"), "id: rice-pub-0001\nsuite: regression\n")
	writeFile(t, filepath.Join(suites, HoldoutDir, "protect", "rice-ho-prot-9001.yaml"), "id: rice-ho-prot-9001\nsuite: holdout\n")
	hoDir := filepath.Join(suites, HoldoutDir)
	// 私有仓检出在别处、以符号链接挂进来，名字也不叫 holdout：只能靠与仓库 holdout 目录的真实路径比较识别。
	private := filepath.Join(t.TempDir(), "private-suites")
	writeFile(t, filepath.Join(private, "fert", "rice-ho-fert-9001.yaml"), "id: rice-ho-fert-9001\nsuite: holdout\n")
	linkRepo := t.TempDir()
	linkSuites := filepath.Join(linkRepo, "eval", "suites")
	writeFile(t, filepath.Join(linkSuites, "regression", "pub.yaml"), "id: rice-pub-0002\nsuite: regression\n")
	if err := os.Symlink(private, filepath.Join(linkSuites, HoldoutDir)); err != nil {
		t.Fatal(err)
	}
	// 公开目录里的样本文件链到 holdout 里：同样拒绝。
	leakRepo := t.TempDir()
	leakSuites := filepath.Join(leakRepo, "eval", "suites")
	writeFile(t, filepath.Join(leakSuites, HoldoutDir, "rice-ho-9003.yaml"), "id: rice-ho-9003\nsuite: holdout\n")
	writeFile(t, filepath.Join(leakSuites, "regression", "pub.yaml"), "id: rice-pub-0003\nsuite: regression\n")
	if err := os.Symlink(filepath.Join(leakSuites, HoldoutDir, "rice-ho-9003.yaml"), filepath.Join(leakSuites, "regression", "leak.yaml")); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name, root, holdoutDir string
		wantIDs                []string // nil 表示期望 ErrHoldoutDenied
	}{
		{name: "root inside holdout", root: filepath.Join(hoDir, "protect"), holdoutDir: hoDir},
		{name: "root inside holdout, no repo dir given", root: filepath.Join(hoDir, "protect")},
		{name: "root is parent of suites", root: filepath.Join(repo, "eval"), holdoutDir: hoDir, wantIDs: []string{"rice-pub-0001"}},
		{name: "root is parent, no repo dir given", root: filepath.Join(repo, "eval"), wantIDs: []string{"rice-pub-0001"}},
		{name: "symlinked private checkout inside", root: filepath.Join(private, "fert"), holdoutDir: filepath.Join(linkSuites, HoldoutDir)},
		{name: "symlinked private checkout from parent", root: linkRepo, holdoutDir: filepath.Join(linkSuites, HoldoutDir), wantIDs: []string{"rice-pub-0002"}},
		{name: "sample file symlinked into holdout", root: leakSuites, holdoutDir: filepath.Join(leakSuites, HoldoutDir)},
	}
	if caseInsensitiveFS(t, repo) {
		cases = append(cases,
			struct {
				name, root, holdoutDir string
				wantIDs                []string
			}{name: "case variant root", root: filepath.Join(suites, "Holdout"), holdoutDir: hoDir},
			struct {
				name, root, holdoutDir string
				wantIDs                []string
			}{name: "case variant repo dir", root: filepath.Join(suites, "HOLDOUT", "protect"), holdoutDir: filepath.Join(suites, "Holdout")})
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			samples, problems, err := Load(c.root, LoadOptions{HoldoutDir: c.holdoutDir})
			if c.wantIDs == nil {
				if !errors.Is(err, ErrHoldoutDenied) || samples != nil || problems != nil {
					t.Fatalf("Load = %d samples, %v; want ErrHoldoutDenied and nothing read", len(samples), err)
				}
				return
			}
			if err != nil || len(problems) != 0 {
				t.Fatalf("Load = %v, problems %v", err, problems)
			}
			var ids []string
			for _, s := range samples {
				ids = append(ids, s.ID)
			}
			if strings.Join(ids, ",") != strings.Join(c.wantIDs, ",") {
				t.Fatalf("ids = %v, want %v", ids, c.wantIDs)
			}
		})
	}
}

// caseInsensitiveFS 报告 dir 所在文件系统是否大小写不敏感（macOS APFS 默认如此）。
func caseInsensitiveFS(t *testing.T, dir string) bool {
	t.Helper()
	p := filepath.Join(dir, "CaseProbe")
	writeFile(t, p, "")
	_, err := os.Stat(filepath.Join(dir, "caseprobe"))
	if rmErr := os.Remove(p); rmErr != nil {
		t.Fatal(rmErr)
	}
	return err == nil
}

// 授权读入时按路径打 holdout 标（含 root 本身在 holdout 内）。
func TestLoad_TagsHoldoutByPath(t *testing.T) {
	root := holdoutTree(t)
	g, err := AuthorizeHoldout(writeReaders(t, readersYAML), "qa-lead")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []string{root, filepath.Join(root, HoldoutDir, "protect")} {
		samples, _, err := Load(r, LoadOptions{HoldoutDir: filepath.Join(root, HoldoutDir), Grant: g})
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range samples {
			if want := s.ID != "rice-pub-0001"; s.Holdout != want {
				t.Errorf("root %s: %s Holdout = %v, want %v", r, s.ID, s.Holdout, want)
			}
		}
	}
}

func TestAuthorizeHoldout(t *testing.T) {
	good := writeReaders(t, readersYAML)
	cases := []struct {
		name, readers, identity string
		denied                  bool // true：ErrHoldoutDenied；false 且 wantErr：其他错误
		wantErr                 bool
	}{
		{name: "owner", readers: good, identity: "qa-lead"},
		{name: "ci", readers: good, identity: "ci-eval"},
		{name: "no identity", readers: good, identity: "", denied: true, wantErr: true},
		{name: "not listed", readers: good, identity: "prompt-tuner", denied: true, wantErr: true},
		{name: "case differs", readers: good, identity: "QA-LEAD", denied: true, wantErr: true},
		{name: "missing file", readers: filepath.Join(t.TempDir(), "nope.yaml"), identity: "qa-lead", wantErr: true},
		{name: "unknown role", readers: writeReaders(t, "readers:\n  - id: qa-lead\n    role: tuner\n"), identity: "qa-lead", wantErr: true},
		{name: "unknown field", readers: writeReaders(t, "readers:\n  - id: qa-lead\n    role: owner\n    note: x\n"), identity: "qa-lead", wantErr: true},
		{name: "empty list", readers: writeReaders(t, "readers: []\n"), identity: "qa-lead", wantErr: true},
		{name: "duplicate", readers: writeReaders(t, readersYAML+"  - id: qa-lead\n    role: ci\n"), identity: "qa-lead", wantErr: true},
		{name: "no owner", readers: writeReaders(t, "readers:\n  - id: ci-eval\n    role: ci\n"), identity: "ci-eval", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g, err := AuthorizeHoldout(c.readers, c.identity)
			if !c.wantErr {
				if err != nil || g.Reader() != c.identity {
					t.Fatalf("grant = %v, %v; want reader %q", g, err, c.identity)
				}
				return
			}
			if err == nil || g != nil {
				t.Fatalf("want error, got grant %v", g)
			}
			if errors.Is(err, ErrHoldoutDenied) != c.denied {
				t.Fatalf("err = %v, denied want %v", err, c.denied)
			}
		})
	}
}

// 非名单身份读取 holdout 被拒（验收 4 前半，失败关闭）。
func TestLoadDirWithHoldout_Denied(t *testing.T) {
	root := holdoutTree(t)
	for name, g := range map[string]*HoldoutGrant{"nil": nil, "zero value": {}} {
		t.Run(name, func(t *testing.T) {
			samples, problems, err := LoadDirWithHoldout(root, g)
			if !errors.Is(err, ErrHoldoutDenied) || samples != nil || problems != nil {
				t.Fatalf("got %d samples, %v; want ErrHoldoutDenied and nothing read", len(samples), err)
			}
		})
	}
	if _, err := LoadAndValidateWithHoldout(root, NewValidator(), nil); !errors.Is(err, ErrHoldoutDenied) {
		t.Fatalf("LoadAndValidateWithHoldout(nil) = %v, want ErrHoldoutDenied", err)
	}
}

func TestLoadDirWithHoldout_Authorized(t *testing.T) {
	root := holdoutTree(t)
	g, err := AuthorizeHoldout(writeReaders(t, readersYAML), "qa-lead")
	if err != nil {
		t.Fatal(err)
	}
	samples, problems, err := LoadDirWithHoldout(root, g)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 2 || len(problems) != 1 {
		t.Fatalf("samples=%d problems=%d, want 2 and 1", len(samples), len(problems))
	}
	if !IsHoldoutPath(root, samples[0].Path) && !IsHoldoutPath(root, samples[1].Path) {
		t.Fatal("holdout sample path not recognised")
	}
	if !strings.Contains(problems[0].Path, HoldoutDir) {
		t.Fatalf("problem path = %s", problems[0].Path)
	}
}

func TestIsHoldoutPath(t *testing.T) {
	root := filepath.Join("eval", "suites")
	cases := map[string]bool{
		filepath.Join(root, "holdout", "protect", "a.yaml"):     true,
		filepath.Join(root, "holdout", "a.yaml"):                true,
		filepath.Join(root, "regression", "a.yaml"):             false,
		filepath.Join(root, "holdoutx", "a.yaml"):               false,
		filepath.Join(root, "capability", "holdout", "a.yaml"):  true, // 任一层叫 holdout 都算（失败关闭，与 Load 一致）
		filepath.Join(root, "Holdout", "a.yaml"):                true, // 大小写不敏感
		filepath.Join("other", "suites", "holdout", "a.yaml"):   false,
		filepath.Join(root, "regression", "..", "holdout", "a"): true,
		filepath.Join(root, "holdout", "..", "regression", "a"): false,
	}
	for p, want := range cases {
		if got := IsHoldoutPath(root, p); got != want {
			t.Errorf("IsHoldoutPath(%q) = %v, want %v", p, got, want)
		}
	}
}

// L2 评审 P1-2：holdout 目录下的文件必须 suite: holdout，反之 suite: holdout 必须在 holdout 目录下。
func TestCheckHoldout(t *testing.T) {
	cases := []struct {
		name    string
		s       Sample
		wantBad bool
	}{
		{"holdout file, holdout suite", Sample{Path: "eval/suites/holdout/protect/a.yaml", Suite: HoldoutDir, Holdout: true}, false},
		{"public file, public suite", Sample{Path: "eval/suites/regression/a.yaml", Suite: "regression"}, false},
		{"holdout file claims regression", Sample{Path: "eval/suites/holdout/regression/a.yaml", Suite: "regression", Holdout: true}, true},
		{"public file claims holdout", Sample{Path: "eval/suites/regression/holdoutish/a.yaml", Suite: HoldoutDir}, true},
		{"parsed only, no path", Sample{Suite: HoldoutDir}, false},
	}
	for _, c := range cases {
		if got := len(checkHoldout(&c.s)) > 0; got != c.wantBad {
			t.Errorf("%s: violation = %v, want %v", c.name, got, c.wantBad)
		}
	}
	found := false
	for _, r := range DefaultRules(mustTaxonomy(t), repoKB(t)) {
		found = found || r.Name() == RuleHoldout
	}
	if !found {
		t.Fatal("RuleHoldout not in DefaultRules")
	}
}
