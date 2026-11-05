package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestOwnersFor(t *testing.T) {
	rules, err := parseCodeowners(strings.NewReader("# comment\n* @all\n/eval/signoffs/ @eval-owner @qa\n\n/eval/signoffs/9.md @solo\n"))
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"eval/signoffs/7.md": "@eval-owner @qa",
		"eval/signoffs/9.md": "@solo",
		"cmd/x.go":           "@all",
	}
	for path, want := range cases {
		if got := strings.Join(ownersFor(rules, path), " "); got != want {
			t.Errorf("%s: owners = %q, want %q", path, got, want)
		}
	}
	if _, err := parseCodeowners(strings.NewReader("/eval/signoffs/\n")); err == nil {
		t.Fatal("pattern without owner accepted")
	}
}

// sigRepo 是签字校验用的临时 git 仓库（分支 main 为信任 ref）：CODEOWNERS 指定 @owner 管 eval/signoffs/，
// owners.yaml 映射账号与 user_id。
type sigRepo struct {
	t   *testing.T
	dir string
}

// sigTrustRef 是测试仓库的信任 ref（生产缺省 origin/main）。
const sigTrustRef = "main"

const sigOwners = "owners:\n  - handle: \"@owner\"\n    email: owner@example.com\n    user_id: 42\n" +
	"  - handle: \"@other\"\n    email: other@example.com\n    user_id: 43\n"

func newSigRepo(t *testing.T) *sigRepo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	r := &sigRepo{t: t, dir: t.TempDir()}
	r.git("", "init", "-q", "-b", sigTrustRef)
	r.write(".github/CODEOWNERS", "/eval/signoffs/ @owner\n")
	r.write("eval/gates/owners.yaml", sigOwners)
	r.git("owner@example.com", "add", ".")
	r.git("owner@example.com", "commit", "-q", "-m", "init")
	return r
}

func (r *sigRepo) write(rel, body string) {
	r.t.Helper()
	p := filepath.Join(r.dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *sigRepo) git(email string, args ...string) {
	r.t.Helper()
	if email == "" {
		email = "t@example.com"
	}
	pre := []string{"-C", r.dir, "-c", "user.name=t", "-c", "user.email=" + email, "-c", "commit.gpgsign=false"}
	if out, err := exec.Command("git", append(pre, args...)...).CombinedOutput(); err != nil {
		r.t.Fatalf("git %v: %v %s", args, err, out)
	}
}

func (r *sigRepo) commit(email, rel, body string) {
	r.t.Helper()
	r.write(rel, body)
	r.git(email, "add", rel)
	r.git(email, "commit", "-q", "-m", "signoff")
}

func (r *sigRepo) cfg() signoffConfig {
	return signoffConfig{repo: r.dir, trustRef: sigTrustRef, dir: "eval/signoffs", codeowners: ".github/CODEOWNERS",
		owners: "eval/gates/owners.yaml"}
}

const goodSignoff = "experiment_id: 7\n\n成本上升 12% 可接受：构造签字。\n"

// 签字文件须在信任 ref（main）的树里且与工作区一致、改过它的提交都是信任 ref 的祖先；CODEOWNERS / owners.yaml
// 取信任 ref 的版本（被测分支改不了）；全部提交作者都是该路径的 CODEOWNERS owner；首行写明 experiment_id、有正文。
func TestVerifySignoff(t *testing.T) {
	const rel = "eval/signoffs/7.md"
	passes := []struct {
		name string
		prep func(r *sigRepo)
	}{
		{"owner signed on main", func(r *sigRepo) { r.commit("owner@example.com", rel, goodSignoff) }},
		{"branch at merged signoff", func(r *sigRepo) {
			r.git("", "checkout", "-q", "-b", "feat")
			r.commit("owner@example.com", rel, goodSignoff)
			r.git("", "checkout", "-q", sigTrustRef)
			r.git("", "merge", "-q", "--ff-only", "feat")
			r.git("", "checkout", "-q", "feat")
		}},
	}
	for _, p := range passes {
		t.Run(p.name, func(t *testing.T) {
			r := newSigRepo(t)
			p.prep(r)
			s, err := verifySignoff(t.Context(), r.cfg(), 7)
			if err != nil || s.UserID != 42 || s.Handle != "@owner" {
				t.Fatalf("signer = %+v err = %v", s, err)
			}
		})
	}
	fails := []struct {
		name, want string // want：报错须含的原因，确认按预期原因失败
		prep       func(r *sigRepo)
	}{
		{"missing file", "read", func(*sigRepo) {}},
		{"uncommitted", "not in trust ref", func(r *sigRepo) { r.write(rel, goodSignoff) }},
		{"dirty after commit", "differs from", func(r *sigRepo) {
			r.commit("owner@example.com", rel, goodSignoff)
			r.write(rel, goodSignoff+"改动\n")
		}},
		{"author not codeowner", "not a CODEOWNERS owner", func(r *sigRepo) { r.commit("other@example.com", rel, goodSignoff) }},
		{"author unknown", "not in owners.yaml", func(r *sigRepo) { r.commit("nobody@example.com", rel, goodSignoff) }},
		{"later edit by non-owner", "not a CODEOWNERS owner", func(r *sigRepo) {
			r.commit("owner@example.com", rel, goodSignoff)
			r.commit("other@example.com", rel, goodSignoff+"补充\n")
		}},
		{"wrong experiment", "first line", func(r *sigRepo) { r.commit("owner@example.com", rel, "experiment_id: 8\n\n正文\n") }},
		{"empty body", "no signoff reason", func(r *sigRepo) { r.commit("owner@example.com", rel, "experiment_id: 7\n\n") }},
		// P0：签字只在被测分支上（未进信任 ref）。
		{"signoff only on branch", "not in trust ref", func(r *sigRepo) {
			r.git("", "checkout", "-q", "-b", "feat")
			r.commit("owner@example.com", rel, goodSignoff)
		}},
		// P0：分支改写已在 main 的签字后又改回原文：内容一致，但改过它的提交不是信任 ref 的祖先。
		{"branch rewrote and restored signoff", "changing", func(r *sigRepo) {
			r.commit("owner@example.com", rel, goodSignoff)
			r.git("", "checkout", "-q", "-b", "feat")
			r.commit("owner@example.com", rel, goodSignoff+"x\n")
			r.commit("owner@example.com", rel, goodSignoff)
		}},
		// P0：信任根来自被测分支：分支把 @other 加进 CODEOWNERS / owners，main 上的 @other 签字仍不算。
		{"branch widens CODEOWNERS", "not a CODEOWNERS owner", func(r *sigRepo) {
			r.commit("other@example.com", rel, goodSignoff)
			r.git("", "checkout", "-q", "-b", "feat")
			r.commit("other@example.com", ".github/CODEOWNERS", "/eval/signoffs/ @owner @other\n")
		}},
		{"branch adds owner entry", "not in owners.yaml", func(r *sigRepo) {
			r.commit("evil@example.com", rel, goodSignoff)
			r.git("", "checkout", "-q", "-b", "feat")
			r.commit("evil@example.com", "eval/gates/owners.yaml", strings.Replace(sigOwners, "owner@example.com", "evil@example.com", 1))
		}},
		// P1：空作者邮箱按校验失败（旧实现按空白切分会吞掉空邮箱，只剩 owner 一个作者而放行）。
		{"empty author email", "empty author email", func(r *sigRepo) {
			r.commit("owner@example.com", rel, goodSignoff)
			r.write(rel, goodSignoff+"补充\n")
			r.git("owner@example.com", "add", rel)
			r.git("owner@example.com", "commit", "-q", "-m", "signoff", "--author=X <>")
		}},
		// require_signed_commits：未签名提交不通过（%G? 须为 G）。
		{"unsigned commit when required", "want G", func(r *sigRepo) {
			r.commit("owner@example.com", "eval/gates/owners.yaml", "require_signed_commits: true\n"+sigOwners)
			r.commit("owner@example.com", rel, goodSignoff)
		}},
	}
	for _, f := range fails {
		t.Run(f.name, func(t *testing.T) {
			r := newSigRepo(t)
			f.prep(r)
			s, err := verifySignoff(t.Context(), r.cfg(), 7)
			if !errors.Is(err, errSignoff) || !strings.Contains(err.Error(), f.want) {
				t.Fatalf("signer = %+v err = %v, want errSignoff containing %q", s, err, f.want)
			}
		})
	}
}

// 信任 ref 不存在或以 - 开头（选项注入）：失败关闭。
func TestVerifySignoff_BadTrustRef(t *testing.T) {
	r := newSigRepo(t)
	r.commit("owner@example.com", "eval/signoffs/7.md", goodSignoff)
	for _, ref := range []string{"", "-main", "nope"} {
		c := r.cfg()
		c.trustRef = ref
		if s, err := verifySignoff(t.Context(), c, 7); !errors.Is(err, errSignoff) {
			t.Errorf("trust ref %q: signer = %+v err = %v", ref, s, err)
		}
	}
}

// 作者核对（纯函数）：require_signed_commits 时只认 %G? = G；不要求时不看签名；空邮箱失败。
func TestCheckCommits(t *testing.T) {
	rules := []codeownersRule{{pattern: "/eval/signoffs/", owners: []string{"@owner"}}}
	owners := &ownersFile{Owners: []ownerEntry{{Handle: "@owner", Email: "owner@example.com", UserID: 42}}}
	const rel = "eval/signoffs/7.md"
	signed := []fileCommit{{Hash: "a", Email: "owner@example.com", Sig: "G"}}
	unsigned := []fileCommit{{Hash: "a", Email: "owner@example.com", Sig: "N"}}
	if s, err := checkCommits(rules, owners, rel, unsigned); err != nil || s.UserID != 42 {
		t.Fatalf("unsigned not required: %+v %v", s, err)
	}
	owners.RequireSignedCommits = true
	if s, err := checkCommits(rules, owners, rel, signed); err != nil || s.UserID != 42 {
		t.Fatalf("signed required: %+v %v", s, err)
	}
	for _, cs := range [][]fileCommit{unsigned, {{Hash: "a", Email: "owner@example.com", Sig: "B"}},
		{{Hash: "a", Email: "", Sig: "G"}}, nil} {
		if _, err := checkCommits(rules, owners, rel, cs); !errors.Is(err, errSignoff) {
			t.Errorf("commits %+v accepted: %v", cs, err)
		}
	}
}

// git log 解析：NUL 分隔，空邮箱保留为空字段（不被吞掉、不错位）。
func TestParseFileCommits(t *testing.T) {
	got, err := parseFileCommits("h1\x00a@x\x00N\x00h2\x00\x00G")
	if err != nil || len(got) != 2 || got[1] != (fileCommit{Hash: "h2", Email: "", Sig: "G"}) || got[0].Email != "a@x" {
		t.Fatalf("got %+v err %v", got, err)
	}
	if got, err := parseFileCommits(""); err != nil || len(got) != 0 {
		t.Fatalf("empty: %+v %v", got, err)
	}
	if _, err := parseFileCommits("h1\x00a@x"); err == nil {
		t.Fatal("truncated record accepted")
	}
}

// 仓库里的 CODEOWNERS / owners.yaml 须能解析且互相对得上（签字路径的每个 CODEOWNERS owner 都在 owners.yaml 中）。
func TestRepoOwnersConsistent(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot, defaultCodeowners))
	if err != nil {
		t.Fatal(err)
	}
	rules, err := parseCodeowners(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	handles := ownersFor(rules, filepath.ToSlash(filepath.Join(defaultSignoffs, "1.md")))
	if len(handles) == 0 {
		t.Fatal("no CODEOWNERS owner for signoffs")
	}
	owners, err := loadOwners(filepath.Join(repoRoot, defaultOwners))
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range handles {
		if !owners.hasHandle(h) {
			t.Errorf("CODEOWNERS owner %s missing in %s", h, defaultOwners)
		}
	}
}
