package llm

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// 分层提示词文件（prompts/{version}/），按此顺序拼接为 system：角色 → 边界 → 输出格式 → 拒答。
var layerFiles = []string{"role.md", "boundary.md", "output_format.md", "refusal.md"}

// tasksDir 是任务提示词子目录：prompts/{version}/tasks/{task}.md。
const tasksDir = "tasks"

// ErrUnknownTask 表示任务没有对应提示词。
var ErrUnknownTask = errors.New("llm: unknown task")

// Prompts 是一个版本的分层提示词，加载后只读。
type Prompts struct {
	version string
	layers  string
	tasks   map[Task]string
}

// LoadPrompts 读取 dir/version 下的分层提示词与全部任务提示词；缺文件或空文件报错。
func LoadPrompts(dir, version string) (*Prompts, error) {
	if version == "" || strings.ContainsAny(version, `/\`) || version == ".." {
		return nil, fmt.Errorf("llm: bad prompt version %q", version)
	}
	root := filepath.Join(dir, version)
	var parts []string
	for _, f := range layerFiles {
		s, err := readPrompt(filepath.Join(root, f))
		if err != nil {
			return nil, err
		}
		parts = append(parts, s)
	}
	p := &Prompts{version: version, layers: strings.Join(parts, "\n\n"), tasks: make(map[Task]string, len(Tasks))}
	for _, t := range Tasks {
		s, err := readPrompt(filepath.Join(root, tasksDir, string(t)+".md"))
		if err != nil {
			return nil, err
		}
		p.tasks[t] = s
	}
	return p, nil
}

// NewPrompts 由内存内容构造（测试用）；tasks 须覆盖 Tasks。
func NewPrompts(version, layers string, tasks map[Task]string) (*Prompts, error) {
	p := &Prompts{version: version, layers: layers, tasks: make(map[Task]string, len(tasks))}
	for _, t := range Tasks {
		s, ok := tasks[t]
		if !ok || strings.TrimSpace(s) == "" {
			return nil, fmt.Errorf("%w: %s has no prompt", ErrUnknownTask, t)
		}
		p.tasks[t] = s
	}
	return p, nil
}

func readPrompt(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("llm: read prompt: %w", err)
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "", fmt.Errorf("llm: prompt %s is empty", path)
	}
	return s, nil
}

// Version 返回提示词版本（如 v1）。
func (p *Prompts) Version() string { return p.version }

// System 返回 task 的 system 提示词：分层提示词 + 任务提示词。
func (p *Prompts) System(t Task) (string, error) {
	s, ok := p.tasks[t]
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrUnknownTask, t)
	}
	return p.layers + "\n\n" + s, nil
}
