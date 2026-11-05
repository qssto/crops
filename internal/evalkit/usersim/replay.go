package usersim

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

var (
	// ErrNoRecording 表示 (sample_id, turn, conv_hash) 没有录制：该 trial 记 infra_error，不编造回复。
	ErrNoRecording = errors.New("usersim: no recording")
	// ErrBadRecording 表示录制文件格式错误、sample_id 与文件名不符、字段非法或键重复。
	ErrBadRecording = errors.New("usersim: bad recording")
)

// convHashPattern 是 ConvHash 的格式（小写 hex sha256）。
var convHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Recording 是一条模拟用户录制。
type Recording struct {
	Turn     int    `yaml:"turn"`
	ConvHash string `yaml:"conv_hash"`
	Reply    string `yaml:"reply"`
}

// recordingFile 是 {dir}/{sample_id}.yaml：一个样本的全部录制。
type recordingFile struct {
	SampleID   string      `yaml:"sample_id"`
	Recordings []Recording `yaml:"recordings"`
}

type recKey struct {
	sampleID string
	turn     int
	convHash string
}

// Replay 是录制回放客户端（不访问模型服务；待真实模型）。
type Replay struct {
	recs map[recKey]string
}

var _ Client = (*Replay)(nil)

// LoadReplay 读 dir/*.yaml（目录不存在视为无录制）。
func LoadReplay(dir string) (*Replay, error) {
	r := &Replay{recs: map[recKey]string{}}
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, fmt.Errorf("usersim: list recordings: %w", err)
	}
	for _, path := range files {
		if err := r.loadFile(path); err != nil {
			return nil, err
		}
	}
	return r, nil
}

func (r *Replay) loadFile(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("usersim: read %s: %w", path, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var f recordingFile
	if err := dec.Decode(&f); err != nil {
		return fmt.Errorf("%w: %s: %v", ErrBadRecording, path, err)
	}
	if want := strings.TrimSuffix(filepath.Base(path), ".yaml"); f.SampleID != want {
		return fmt.Errorf("%w: %s: sample_id %q does not match file name", ErrBadRecording, path, f.SampleID)
	}
	for i, rec := range f.Recordings {
		at := fmt.Sprintf("%s: recordings[%d]", path, i)
		switch {
		case rec.Turn < 1:
			return fmt.Errorf("%w: %s: turn must be >= 1 (turn 0 is the sample question)", ErrBadRecording, at)
		case !convHashPattern.MatchString(rec.ConvHash):
			return fmt.Errorf("%w: %s: conv_hash must be 64 lowercase hex", ErrBadRecording, at)
		case strings.TrimSpace(rec.Reply) == "":
			return fmt.Errorf("%w: %s: reply is empty", ErrBadRecording, at)
		}
		k := recKey{sampleID: f.SampleID, turn: rec.Turn, convHash: rec.ConvHash}
		if _, dup := r.recs[k]; dup {
			return fmt.Errorf("%w: %s: duplicate (turn, conv_hash)", ErrBadRecording, at)
		}
		r.recs[k] = rec.Reply
	}
	return nil
}

// Reply 实现 Client：按 (sample_id, turn, conv_hash) 回放；无录制返回 ErrNoRecording。
func (r *Replay) Reply(_ context.Context, req Request) (string, error) {
	reply, ok := r.recs[recKey{sampleID: req.SampleID, turn: req.Turn, convHash: req.ConvHash}]
	if !ok {
		return "", fmt.Errorf("%w for (%s, turn %d, %s)", ErrNoRecording, req.SampleID, req.Turn, req.ConvHash)
	}
	return reply, nil
}
