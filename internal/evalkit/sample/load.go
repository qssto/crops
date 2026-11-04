package sample

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// RuleParse 是解析失败的违规规则名。
const RuleParse = "parse"

var sampleExts = map[string]bool{".yaml": true, ".yml": true}

// IsSampleFile 判断文件名是否为样本 YAML（.yaml / .yml）。
func IsSampleFile(name string) bool { return sampleExts[strings.ToLower(filepath.Ext(name))] }

// LoadOptions 是 Load 的参数。
type LoadOptions struct {
	// HoldoutDir 是仓库的 holdout 目录（<repo>/eval/suites/holdout），按真实目录比较（见 holdoutGuard）；
	// 为空或不存在时只按目录名兜底。
	HoldoutDir string
	// Grant 非 nil 时连 holdout 一起读，须是 AuthorizeHoldout 签发的凭证，否则返回 ErrHoldoutDenied、不读任何文件。
	Grant *HoldoutGrant
}

// LoadDir 递归加载 root 下全部样本 YAML（eval/suites/**.yaml），按路径排序；holdout 只按目录名识别，
// 知道仓库根目录时用 Load 并给 HoldoutDir。
func LoadDir(root string) ([]*Sample, []Violation, error) { return Load(root, LoadOptions{}) }

// LoadDirWithHoldout 同 LoadDir，但连 holdout 一起读；g 须是 AuthorizeHoldout 签发的凭证，否则返回 ErrHoldoutDenied、不读任何文件。
func LoadDirWithHoldout(root string, g *HoldoutGrant) ([]*Sample, []Violation, error) {
	if !g.valid() {
		return nil, nil, ErrHoldoutDenied
	}
	return Load(root, LoadOptions{Grant: g})
}

// Load 递归加载 root 下全部样本 YAML，按路径排序。单个文件解析失败记为 parse 违规、不中断加载；目录读取失败返回 error。
// 无凭证时（L2 评审 P1-1）：root 解析符号链接后在 holdout 内部 → ErrHoldoutDenied；root 是 holdout 祖先 → 跳过 holdout 子树、
// 不打开其中任何文件；样本文件是链进 holdout 的符号链接 → ErrHoldoutDenied。
// 有凭证时全部读入，holdout 内的样本按路径打 Holdout 标（不看 suite 字段）。
func Load(root string, o LoadOptions) ([]*Sample, []Violation, error) {
	allow := o.Grant != nil
	if allow && !o.Grant.valid() {
		return nil, nil, ErrHoldoutDenied
	}
	guard, err := newHoldoutGuard(o.HoldoutDir)
	if err != nil {
		return nil, nil, err
	}
	rootInside, err := guard.within(root)
	if err != nil {
		return nil, nil, err
	}
	if rootInside && !allow {
		return nil, nil, fmt.Errorf("%w: %s is inside holdout", ErrHoldoutDenied, root)
	}
	files, err := walkSamples(root, guard, allow)
	if err != nil {
		return nil, nil, err
	}

	samples := make([]*Sample, 0, len(files))
	var problems []Violation
	for _, f := range files {
		raw, err := os.ReadFile(f.path)
		if err != nil {
			return nil, nil, fmt.Errorf("read %s: %w", f.path, err)
		}
		s, err := Parse(raw)
		if err != nil {
			problems = append(problems, Violation{Path: f.path, Rule: RuleParse, Msg: err.Error()})
			continue
		}
		s.Path = f.path
		s.Raw = raw
		s.Holdout = rootInside || f.holdout
		samples = append(samples, s)
	}
	return samples, problems, nil
}

type sampleFile struct {
	path    string
	holdout bool
}

// walkSamples 列出 root 下的样本文件（按路径排序）；allow 为假时跳过 holdout 子树，遇到链进 holdout 的样本文件即拒绝。
func walkSamples(root string, guard holdoutGuard, allow bool) ([]sampleFile, error) {
	var files []sampleFile
	var hoDirs []string // allow 时记下 holdout 目录，给其下文件打标
	under := func(path string) bool {
		for _, d := range hoDirs {
			if strings.HasPrefix(path, d+string(filepath.Separator)) {
				return true
			}
		}
		return false
	}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			fi, err := d.Info()
			if err != nil {
				return err
			}
			if guard.match(path, fi) {
				if !allow {
					return fs.SkipDir
				}
				hoDirs = append(hoDirs, path)
			}
			return nil
		}
		if !IsSampleFile(path) {
			return nil
		}
		ho := under(path)
		if d.Type()&fs.ModeSymlink != 0 && !ho {
			in, err := guard.within(path)
			if err != nil {
				return err
			}
			if in && !allow {
				return fmt.Errorf("%w: %s links into holdout", ErrHoldoutDenied, path)
			}
			ho = in
		}
		files = append(files, sampleFile{path: path, holdout: ho})
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrHoldoutDenied) {
			return nil, err
		}
		return nil, fmt.Errorf("walk %s: %w", root, err)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].path < files[j].path })
	return files, nil
}

// Parse 严格解析单个样本：未知字段（多半是拼写错误）直接报错。
func Parse(raw []byte) (*Sample, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	s := &Sample{}
	if err := dec.Decode(s); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("empty sample file")
		}
		return nil, fmt.Errorf("decode sample: %w", err)
	}
	return s, nil
}
