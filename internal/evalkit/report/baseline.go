package report

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"time"

	"gopkg.in/yaml.v3"

	"agri-steward/internal/evalkit"
	"agri-steward/internal/evalkit/reduce"
)

// baseline 对象与内容类型（P0 §6.9 功能任务 4：报告 + 配置快照上传 report/{exp_id}/）。
const (
	reportPrefix        = "report"
	SnapshotFile        = "snapshot.json"
	ReportContentType   = "text/markdown; charset=utf-8"
	SnapshotContentType = "application/json"
	baselineExt         = ".yaml"

	// noteStubModel 是桩模型 baseline 的如实标注（简报 L8 共同规则）。
	noteStubModel = "model: stub —— 真实模型未验证（ONEAPI_* 未设置），本 baseline 只反映桩模型下的编排、护栏与打分器行为"
	noteSignoff   = "Gate 0 #6 需评测负责人签字：待签字"
	noteManual    = "结论一致性：待人工抽检"
)

// baselineName 限制 baseline 名只用小写字母、数字与 . _ -（直接作为文件名）。
var baselineName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// ReportKey 是报告在 eval bucket 内的对象键：report/{exp_id}/report.md。
func ReportKey(expID string) string { return path.Join(reportPrefix, expID, FileName) }

// SnapshotKey 是配置快照的对象键：report/{exp_id}/snapshot.json。
func SnapshotKey(expID string) string { return path.Join(reportPrefix, expID, SnapshotFile) }

// Baseline 是 eval/baselines/{name}.yaml 的内容：之后的实验与它配对比较（评测设计 §8.4）。
type Baseline struct {
	Name        string           `yaml:"name"`
	ExpID       string           `yaml:"exp_id"`
	GeneratedAt time.Time        `yaml:"generated_at"`
	Model       string           `yaml:"model"`
	Bucket      string           `yaml:"bucket"`
	ReportKey   string           `yaml:"report_key"`
	SnapshotKey string           `yaml:"snapshot_key"`
	Snapshot    evalkit.Snapshot `yaml:"snapshot"`
	Suites      []string         `yaml:"suites"`
	Tags        []string         `yaml:"tags,omitempty"`
	N           int              `yaml:"n"`
	Samples     int              `yaml:"samples"`
	Trials      int              `yaml:"trials"`
	// Gate 是报告首行门禁结论（Gate 0 自动项）。
	Gate  string   `yaml:"gate"`
	Notes []string `yaml:"notes"`
}

// snapshotDoc 是 snapshot.json 的内容。
type snapshotDoc struct {
	ExpID    string           `json:"exp_id"`
	Snapshot evalkit.Snapshot `json:"snapshot"`
}

// ValidateBaselineName 校验 baseline 名（作为文件名，cmd 在连库前先校验）。
func ValidateBaselineName(name string) error {
	if !baselineName.MatchString(name) {
		return fmt.Errorf("report: baseline name %q must match %s", name, baselineName)
	}
	return nil
}

// PublishBaseline 把 reportPath 处的报告与配置快照上传到 bucket 的 report/{exp_id}/，再写 {dir}/{name}.yaml，
// 返回 yaml 路径与内容。now 由调用方经 clock 注入。
func PublishBaseline(ctx context.Context, obj ObjectPutter, bucket, dir, name string, exp *evalkit.Experiment,
	reportPath string, now time.Time) (string, *Baseline, error) {
	if err := ValidateBaselineName(name); err != nil {
		return "", nil, err
	}
	if exp.ID == "" || exp.Snapshot.IsZero() {
		return "", nil, fmt.Errorf("report: baseline %s needs experiment id and snapshot", name)
	}
	body, err := os.ReadFile(reportPath)
	if err != nil {
		return "", nil, fmt.Errorf("report: read %s: %w", reportPath, err)
	}
	snap, err := json.MarshalIndent(snapshotDoc{ExpID: exp.ID, Snapshot: exp.Snapshot}, "", "  ")
	if err != nil {
		return "", nil, fmt.Errorf("report: marshal snapshot: %w", err)
	}
	b := NewBaseline(name, bucket, exp, now)
	if err := obj.PutObject(ctx, bucket, b.ReportKey, body, ReportContentType); err != nil {
		return "", nil, fmt.Errorf("report: upload %s: %w", b.ReportKey, err)
	}
	if err := obj.PutObject(ctx, bucket, b.SnapshotKey, snap, SnapshotContentType); err != nil {
		return "", nil, fmt.Errorf("report: upload %s: %w", b.SnapshotKey, err)
	}
	out, err := yaml.Marshal(b)
	if err != nil {
		return "", nil, fmt.Errorf("report: marshal baseline: %w", err)
	}
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return "", nil, fmt.Errorf("report: mkdir %s: %w", dir, err)
	}
	p := filepath.Join(dir, name+baselineExt)
	if err := os.WriteFile(p, out, filePerm); err != nil {
		return "", nil, fmt.Errorf("report: write %s: %w", p, err)
	}
	return p, b, nil
}

// NewBaseline 组装 baseline 记录（不做 I/O）。
func NewBaseline(name, bucket string, exp *evalkit.Experiment, now time.Time) *Baseline {
	notes := []string{noteSignoff, noteManual}
	if exp.Snapshot.Model == evalkit.ModelStub {
		notes = append([]string{noteStubModel}, notes...)
	}
	return &Baseline{Name: name, ExpID: exp.ID, GeneratedAt: now.UTC(), Model: exp.Snapshot.Model, Bucket: bucket,
		ReportKey: ReportKey(exp.ID), SnapshotKey: SnapshotKey(exp.ID), Snapshot: exp.Snapshot,
		Suites: exp.Suites, Tags: exp.Tags, N: exp.N, Samples: exp.Samples, Trials: len(exp.Trials),
		Gate: GateLine(reduce.Reduce(exp.Trials)), Notes: notes}
}
