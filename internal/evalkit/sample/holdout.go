package sample

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// HoldoutDir 是 holdout 套件在样本根目录下的子目录（私有仓以 git submodule 挂到 eval/suites/holdout/，P2 §6.3 技术实现 5）。
// LoadDir 默认跳过它；只有 AuthorizeHoldout 签发的凭证才能读入（私有仓权限 + evalkit 读权限双控）。
const HoldoutDir = "holdout"

// holdout 读者角色：只有 holdout 负责人与 CI 服务账号。
const (
	ReaderRoleOwner = "owner"
	ReaderRoleCI    = "ci"
)

// ErrHoldoutDenied 表示调用者不在 holdout 读者名单（或未给身份），holdout 内容一律不读。
var ErrHoldoutDenied = errors.New("sample: holdout read denied")

// HoldoutReader 是 eval/holdout_readers.yaml 的一项。
type HoldoutReader struct {
	ID   string `yaml:"id"`
	Role string `yaml:"role"`
}

type holdoutReaders struct {
	Readers []HoldoutReader `yaml:"readers"`
}

// HoldoutGrant 是读 holdout 的凭证，只能由 AuthorizeHoldout 签发；零值与 nil 都视为无权限。
type HoldoutGrant struct {
	reader string
}

// Reader 返回凭证持有者身份（nil 为空串）。
func (g *HoldoutGrant) Reader() string {
	if g == nil {
		return ""
	}
	return g.reader
}

func (g *HoldoutGrant) valid() bool { return g != nil && g.reader != "" }

// loadHoldoutReaders 严格解析名单：未知字段、未知角色、空名单、重复 id、没有负责人都报错（失败关闭）。
func loadHoldoutReaders(path string) ([]HoldoutReader, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("holdout readers: %w", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var f holdoutReaders
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("holdout readers %s: %w", path, err)
	}
	seen := make(map[string]bool, len(f.Readers))
	owner := false
	for _, r := range f.Readers {
		switch {
		case r.ID == "":
			return nil, fmt.Errorf("holdout readers %s: empty id", path)
		case r.Role != ReaderRoleOwner && r.Role != ReaderRoleCI:
			return nil, fmt.Errorf("holdout readers %s: %s has role %q, want %s|%s", path, r.ID, r.Role, ReaderRoleOwner, ReaderRoleCI)
		case seen[r.ID]:
			return nil, fmt.Errorf("holdout readers %s: duplicate id %s", path, r.ID)
		}
		seen[r.ID] = true
		owner = owner || r.Role == ReaderRoleOwner
	}
	if !owner {
		return nil, fmt.Errorf("holdout readers %s: no %s listed", path, ReaderRoleOwner)
	}
	return f.Readers, nil
}

// AuthorizeHoldout 校验 identity 在名单 readersPath 中（精确匹配），通过才签发凭证。
// 名单读不到或格式错返回普通错误；身份为空或不在名单返回 ErrHoldoutDenied。
func AuthorizeHoldout(readersPath, identity string) (*HoldoutGrant, error) {
	readers, err := loadHoldoutReaders(readersPath)
	if err != nil {
		return nil, err
	}
	if identity == "" {
		return nil, fmt.Errorf("%w: no reader identity", ErrHoldoutDenied)
	}
	for _, r := range readers {
		if r.ID == identity {
			return &HoldoutGrant{reader: identity}, nil
		}
	}
	return nil, fmt.Errorf("%w: %q is not in %s", ErrHoldoutDenied, identity, readersPath)
}

// checkHoldout（L2 评审 P1-2）：holdout 目录下的文件必须 suite: holdout，suite: holdout 必须在 holdout 目录下；
// 否则 holdout 样本可借公开 suite 名进公开报告与专家队列，或公开样本冒充 holdout。只查加载得来的样本（Path 非空）。
func checkHoldout(s *Sample) []string {
	switch {
	case s.Path == "":
		return nil
	case s.Holdout && s.Suite != HoldoutDir:
		return []string{fmt.Sprintf("file under %s/ must declare suite: %s, got %q", HoldoutDir, HoldoutDir, s.Suite)}
	case !s.Holdout && s.Suite == HoldoutDir:
		return []string{fmt.Sprintf("suite: %s must live under eval/suites/%s/", HoldoutDir, HoldoutDir)}
	}
	return nil
}

// IsHoldoutPath 报告 path 是否在 root 下某个 holdout 目录之内：root 之下任一层目录名不区分大小写等于 holdout
// （按清理后的路径比较，不访问文件；与 Load 的按名兜底一致，失败关闭）。
func IsHoldoutPath(root, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	for _, d := range parts[:len(parts)-1] {
		if strings.EqualFold(d, HoldoutDir) {
			return true
		}
	}
	return false
}

// holdoutGuard 判定目录是否属于 holdout（L2 评审 P1-1，失败关闭）：
//   - 与仓库 holdout 目录（<repo>/eval/suites/holdout）是同一目录：os.SameFile 按设备号 + inode 比较，
//     跟随符号链接，大小写不敏感文件系统上 Holdout / HOLDOUT 也命中，私有仓检出在别处再链进来也命中；
//   - 或目录名不区分大小写等于 holdout（未给仓库目录、未挂载 submodule 时的兜底）。
type holdoutGuard struct{ repo fs.FileInfo }

// newHoldoutGuard 取仓库 holdout 目录的真实目录信息；dir 为空或不存在（未挂载）时只按名兜底。
func newHoldoutGuard(dir string) (holdoutGuard, error) {
	if dir == "" {
		return holdoutGuard{}, nil
	}
	fi, err := os.Stat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return holdoutGuard{}, nil
	case err != nil:
		return holdoutGuard{}, fmt.Errorf("holdout dir %s: %w", dir, err)
	case !fi.IsDir():
		return holdoutGuard{}, fmt.Errorf("holdout dir %s is not a directory", dir)
	}
	return holdoutGuard{repo: fi}, nil
}

// match 报告目录 path（fi 是它的目录信息）是否是 holdout 目录。
func (g holdoutGuard) match(path string, fi fs.FileInfo) bool {
	if strings.EqualFold(filepath.Base(path), HoldoutDir) {
		return true
	}
	return g.repo != nil && fi != nil && os.SameFile(g.repo, fi)
}

// within 报告 path 解析符号链接后本身或任一祖先目录是否是 holdout 目录。
func (g holdoutGuard) within(path string) (bool, error) {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false, fmt.Errorf("resolve %s: %w", path, err)
	}
	p, err := filepath.Abs(real)
	if err != nil {
		return false, fmt.Errorf("resolve %s: %w", path, err)
	}
	for {
		fi, err := os.Stat(p)
		if err != nil {
			return false, fmt.Errorf("stat %s: %w", p, err)
		}
		if fi.IsDir() && g.match(p, fi) {
			return true, nil
		}
		parent := filepath.Dir(p)
		if parent == p {
			return false, nil
		}
		p = parent
	}
}
