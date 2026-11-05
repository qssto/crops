package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	// defaultSignoffs 是 NEED_SIGNOFF 签字文件目录（{experiment_id}.md，入库）。
	defaultSignoffs = "eval/signoffs"
	// defaultCodeowners 是签字人名单来源（作者须是签字目录的 CODEOWNERS owner）。
	defaultCodeowners = ".github/CODEOWNERS"
	// defaultOwners 是 CODEOWNERS 账号 → 提交邮箱 / user_id（signed_by 写 user_id）。
	defaultOwners = "eval/gates/owners.yaml"
	// signoffHeader 是签字文件首行前缀：experiment_id: {id}。
	signoffHeader = "experiment_id:"
	// defaultTrustRef 是签字信任根：签字文件、CODEOWNERS、owners.yaml 都以该 ref 为准。
	defaultTrustRef = "origin/main"
)

// errSignoff 是签字校验不通过（文件缺失 / 不在信任 ref / 作者不是 owner / 内容不符）。
var errSignoff = errors.New("gate: signoff not valid")

// signoffConfig 是签字校验所需配置：trustRef 是信任 ref，dir / codeowners / owners 相对 repo。
type signoffConfig struct {
	repo, trustRef, dir, codeowners, owners string
}

// signer 是通过校验的签字人。
type signer struct {
	Handle string
	UserID uint64
}

// codeownersRule 是 CODEOWNERS 一行：pattern 与 owners。
type codeownersRule struct {
	pattern string
	owners  []string
}

// parseCodeowners 解析 CODEOWNERS（# 注释、空行跳过；每行 pattern + 至少一个 owner）。
func parseCodeowners(r io.Reader) ([]codeownersRule, error) {
	var out []codeownersRule
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if i := strings.Index(line, "#"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		if line == "" {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 2 {
			return nil, fmt.Errorf("gate: CODEOWNERS line %d: pattern %q has no owner", n, f[0])
		}
		out = append(out, codeownersRule{pattern: f[0], owners: f[1:]})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("gate: read CODEOWNERS: %w", err)
	}
	return out, nil
}

// ownersFor 按 GitHub 口径取最后一条匹配规则的 owners。支持的 pattern：*、/dir/（目录前缀）、/path（精确文件），
// 以及不带 / 前缀的同类写法（视为从仓库根起）；签字目录只需这几种。
func ownersFor(rules []codeownersRule, file string) []string {
	file = strings.TrimPrefix(path.Clean("/"+file), "/")
	var owners []string
	for _, r := range rules {
		p := strings.TrimPrefix(r.pattern, "/")
		switch {
		case p == "*":
		case strings.HasSuffix(p, "/"):
			if !strings.HasPrefix(file, p) {
				continue
			}
		case file != p:
			continue
		}
		owners = r.owners
	}
	return owners
}

// ownerEntry 是 owners.yaml 一项（handle 同 CODEOWNERS 写法，如 @eval-owner）。
type ownerEntry struct {
	Handle string `yaml:"handle"`
	Email  string `yaml:"email"`
	UserID uint64 `yaml:"user_id"`
}

type ownersFile struct {
	Notes []string `yaml:"notes"`
	// RequireSignedCommits 为 true 时签字文件的每个提交都须是有效签名（git %G? = G）；缺省 false。
	RequireSignedCommits bool         `yaml:"require_signed_commits"`
	Owners               []ownerEntry `yaml:"owners"`
}

// loadOwners 读并严格解析本地 owners.yaml。
func loadOwners(p string) (*ownersFile, error) {
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("gate: read %s: %w", p, err)
	}
	return parseOwners(raw, p)
}

// parseOwners 严格解析 owners.yaml：handle / email 唯一且非空、user_id > 0。p 只用于报错。
func parseOwners(raw []byte, p string) (*ownersFile, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var f ownersFile
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("gate: parse %s: %w", p, err)
	}
	seen := map[string]bool{}
	for _, o := range f.Owners {
		h, e := "h:"+o.Handle, "e:"+strings.ToLower(o.Email)
		if o.Handle == "" || o.Email == "" || o.UserID == 0 || seen[h] || seen[e] {
			return nil, fmt.Errorf("gate: %s: bad or duplicate owner %+v", p, o)
		}
		seen[h], seen[e] = true, true
	}
	return &f, nil
}

func (f *ownersFile) hasHandle(h string) bool { return f.byHandle(h) != nil }

func (f *ownersFile) byHandle(h string) *ownerEntry {
	for i := range f.Owners {
		if f.Owners[i].Handle == h {
			return &f.Owners[i]
		}
	}
	return nil
}

func (f *ownersFile) byEmail(e string) *ownerEntry {
	for i := range f.Owners {
		if strings.EqualFold(f.Owners[i].Email, e) {
			return &f.Owners[i]
		}
	}
	return nil
}

// verifySignoff 校验 {dir}/{id}.md。信任根是 c.trustRef（缺省 origin/main），不是被测分支：
//   - 文件在信任 ref 的树里，且与工作区内容逐字节一致；
//   - 当前 HEAD 历史里改过该文件的提交都是信任 ref 的祖先（分支上改写再改回也不算）；
//   - CODEOWNERS / owners.yaml 取信任 ref 的版本；信任 ref 历史里该文件的每个提交作者（%ae，非空）都在 owners.yaml 中、
//     handle 是该路径的 CODEOWNERS owner；owners.yaml require_signed_commits 为 true 时每个提交 %G? 须为 G；
//   - 首个非空行为 experiment_id: {id}，其后有正文。
//
// 作者邮箱可伪造：防伪靠 main 分支保护 + 要求签名提交（待部署方）。本地与 CI 同一口径（CI checkout 须 fetch-depth: 0）。
// 返回最近一次提交的作者。
func verifySignoff(ctx context.Context, c signoffConfig, id uint64) (*signer, error) {
	rel := path.Clean(filepath.ToSlash(filepath.Join(c.dir, strconv.FormatUint(id, 10)+".md")))
	body, err := os.ReadFile(filepath.Join(c.repo, filepath.FromSlash(rel)))
	if err != nil {
		return nil, fmt.Errorf("%w: read %s: %w", errSignoff, rel, err)
	}
	if err := checkSignoffBody(string(body), id); err != nil {
		return nil, fmt.Errorf("%w: %s: %w", errSignoff, rel, err)
	}
	trust, err := resolveTrustRef(ctx, c.repo, c.trustRef)
	if err != nil {
		return nil, err
	}
	trusted, err := trustedFile(ctx, c.repo, trust, rel)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(trusted, body) {
		return nil, fmt.Errorf("%w: %s differs from %s", errSignoff, rel, c.trustRef)
	}
	if err := checkAncestry(ctx, c.repo, trust, rel); err != nil {
		return nil, err
	}
	rules, owners, err := trustedOwners(ctx, c, trust)
	if err != nil {
		return nil, err
	}
	log, err := gitOut(ctx, c.repo, "log", "-z", "--format=%H%x00%ae%x00%G?", trust, "--", rel)
	if err != nil {
		return nil, err
	}
	commits, err := parseFileCommits(log)
	if err != nil {
		return nil, err
	}
	return checkCommits(rules, owners, rel, commits)
}

// resolveTrustRef 把信任 ref 解析为提交 SHA；空、以 - 开头（选项注入）或不存在都按未签字。
func resolveTrustRef(ctx context.Context, repo, ref string) (string, error) {
	if ref == "" || strings.HasPrefix(ref, "-") {
		return "", fmt.Errorf("%w: bad trust ref %q", errSignoff, ref)
	}
	sha, err := gitOut(ctx, repo, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("%w: trust ref %s: %w", errSignoff, ref, err)
	}
	return strings.TrimSpace(sha), nil
}

// trustedFile 读信任提交树里的 rel（原始字节）；不在树里按未签字。
func trustedFile(ctx context.Context, repo, trust, rel string) ([]byte, error) {
	obj := trust + ":" + rel
	if _, err := gitOut(ctx, repo, "cat-file", "-e", obj); err != nil {
		return nil, fmt.Errorf("%w: %s not in trust ref: %w", errSignoff, rel, err)
	}
	out, err := gitOut(ctx, repo, "cat-file", "blob", obj)
	if err != nil {
		return nil, err
	}
	return []byte(out), nil
}

// checkAncestry：HEAD 历史里改过 rel 的每个提交都须是信任提交的祖先。
func checkAncestry(ctx context.Context, repo, trust, rel string) error {
	hashes, err := gitOut(ctx, repo, "log", "--format=%H", "HEAD", "--", rel)
	if err != nil {
		return err
	}
	for _, h := range strings.Fields(hashes) {
		_, err := gitOut(ctx, repo, "merge-base", "--is-ancestor", h, trust)
		var exit *exec.ExitError
		switch {
		case err == nil:
		case errors.As(err, &exit) && exit.ExitCode() == 1:
			return fmt.Errorf("%w: commit %s changing %s is not in trust ref", errSignoff, h, rel)
		default:
			return err
		}
	}
	return nil
}

// trustedOwners 从信任提交读 CODEOWNERS 与 owners.yaml（被测分支上的改动不生效）。
func trustedOwners(ctx context.Context, c signoffConfig, trust string) ([]codeownersRule, *ownersFile, error) {
	raw, err := trustedFile(ctx, c.repo, trust, path.Clean(filepath.ToSlash(c.codeowners)))
	if err != nil {
		return nil, nil, err
	}
	rules, err := parseCodeowners(bytes.NewReader(raw))
	if err != nil {
		return nil, nil, err
	}
	raw, err = trustedFile(ctx, c.repo, trust, path.Clean(filepath.ToSlash(c.owners)))
	if err != nil {
		return nil, nil, err
	}
	owners, err := parseOwners(raw, c.owners)
	if err != nil {
		return nil, nil, err
	}
	return rules, owners, nil
}

// fileCommit 是改过签字文件的一个提交：哈希、作者邮箱（%ae）、签名状态（%G?）。
type fileCommit struct {
	Hash, Email, Sig string
}

// parseFileCommits 解析 git log -z --format=%H%x00%ae%x00%G?：字段与记录都以 NUL 分隔（空邮箱保留为空字段）。
func parseFileCommits(out string) ([]fileCommit, error) {
	out = strings.TrimSuffix(out, "\x00")
	if out == "" {
		return nil, nil
	}
	f := strings.Split(out, "\x00")
	if len(f)%3 != 0 {
		return nil, fmt.Errorf("gate: git log: %d fields, want multiple of 3", len(f))
	}
	cs := make([]fileCommit, 0, len(f)/3)
	for i := 0; i < len(f); i += 3 {
		cs = append(cs, fileCommit{Hash: f[i], Email: f[i+1], Sig: f[i+2]})
	}
	return cs, nil
}

// checkCommits 要求 commits（git log 顺序，最近在前）非空，每个作者邮箱非空、在 owners 中且是 rel 的 CODEOWNERS owner；
// owners.RequireSignedCommits 时签名状态须为 G。返回最近一次提交的作者。
func checkCommits(rules []codeownersRule, owners *ownersFile, rel string, commits []fileCommit) (*signer, error) {
	if len(commits) == 0 {
		return nil, fmt.Errorf("%w: %s has no commit", errSignoff, rel)
	}
	allowed := ownersFor(rules, rel)
	var first *signer
	for _, cm := range commits {
		if cm.Email == "" {
			return nil, fmt.Errorf("%w: commit %s of %s has empty author email", errSignoff, cm.Hash, rel)
		}
		o := owners.byEmail(cm.Email)
		if o == nil {
			return nil, fmt.Errorf("%w: author %s of %s not in owners.yaml", errSignoff, cm.Email, rel)
		}
		if !containsString(allowed, o.Handle) {
			return nil, fmt.Errorf("%w: author %s (%s) is not a CODEOWNERS owner of %s (owners %v)", errSignoff, cm.Email, o.Handle, rel, allowed)
		}
		if owners.RequireSignedCommits && cm.Sig != "G" {
			return nil, fmt.Errorf("%w: commit %s of %s signature %q, want G", errSignoff, cm.Hash, rel, cm.Sig)
		}
		if first == nil {
			first = &signer{Handle: o.Handle, UserID: o.UserID}
		}
	}
	return first, nil
}

// checkSignoffBody：首个非空行 experiment_id: {id}，其后至少一行非空正文（签字理由）。
func checkSignoffBody(body string, id uint64) error {
	var lines []string
	for _, l := range strings.Split(body, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) == 0 {
		return errors.New("empty file")
	}
	v, ok := strings.CutPrefix(lines[0], signoffHeader)
	if !ok || strings.TrimSpace(v) != strconv.FormatUint(id, 10) {
		return fmt.Errorf("first line %q, want %s %d", lines[0], signoffHeader, id)
	}
	if len(lines) < 2 {
		return errors.New("no signoff reason after header")
	}
	return nil
}

func containsString(xs []string, s string) bool { return slices.Contains(xs, s) }

// gitOut 在 repo 跑 git 并返回 stdout。
func gitOut(ctx context.Context, repo string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", repo}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}
