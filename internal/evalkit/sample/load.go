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

// LoadDir 递归加载 root 下全部样本 YAML（eval/suites/**.yaml），按路径排序。
// 单个文件解析失败记为 parse 违规、不中断加载；目录读取失败返回 error。
func LoadDir(root string) ([]*Sample, []Violation, error) {
	var paths []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !d.IsDir() && sampleExts[strings.ToLower(filepath.Ext(path))] {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("walk %s: %w", root, err)
	}
	sort.Strings(paths)

	samples := make([]*Sample, 0, len(paths))
	var problems []Violation
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			return nil, nil, fmt.Errorf("read %s: %w", p, err)
		}
		s, err := Parse(raw)
		if err != nil {
			problems = append(problems, Violation{Path: p, Rule: RuleParse, Msg: err.Error()})
			continue
		}
		s.Path = p
		samples = append(samples, s)
	}
	return samples, problems, nil
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
