package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"

	"agri-steward/internal/repository"
)

// manifestFile 是样本清单明细的对象名：agri-steward-eval/{experiment_id}/manifest.json（P2 §6.1 ① 1）。
// baselineNameFile 是冻结时登记的基线名：agri-steward-eval/{experiment_id}/baseline.json（库里没有基线名列）。
const (
	manifestFile        = "manifest.json"
	baselineNameFile    = "baseline.json"
	manifestContentType = "application/json"
)

// ErrBaselineName 表示 pointer-only 补写的名字与冻结时登记的名字对不上，或登记名对象读不到 / 内容不符（失败关闭）。
var ErrBaselineName = errors.New("store: baseline name differs from the registered name")

// ManifestKey 是实验样本清单的对象键。
func ManifestKey(id uint64) string { return path.Join(FormatID(id), manifestFile) }

// Manifest 是样本清单明细：samples 为 sample_id@version 排序列表（其 sha256 即 snapshot.samples_hash）。
type Manifest struct {
	ExperimentID string   `json:"experiment_id"`
	SamplesHash  string   `json:"samples_hash"`
	Samples      []string `json:"samples"`
}

// PutManifest 把样本清单写入 bucket 的 {id}/manifest.json，返回对象键。
func PutManifest(ctx context.Context, obj repository.ObjectPutter, bucket string, id uint64, samplesHash string, refs []string) (string, error) {
	if len(refs) == 0 {
		return "", fmt.Errorf("store: manifest of %d: no samples", id)
	}
	b, err := json.MarshalIndent(Manifest{ExperimentID: FormatID(id), SamplesHash: samplesHash, Samples: refs}, "", "  ")
	if err != nil {
		return "", fmt.Errorf("store: manifest: %w", err)
	}
	key := ManifestKey(id)
	if err := obj.PutObject(ctx, bucket, key, b, manifestContentType); err != nil {
		return "", fmt.Errorf("store: upload %s: %w", key, err)
	}
	return key, nil
}

// BaselineNameKey 是实验登记基线名的对象键。
func BaselineNameKey(id uint64) string { return path.Join(FormatID(id), baselineNameFile) }

// BaselineName 是冻结时登记的基线名。
type BaselineName struct {
	ExperimentID string `json:"experiment_id"`
	Name         string `json:"name"`
}

// PutBaselineName 在冻结前把基线名写入 bucket 的 {id}/baseline.json（对象可覆盖，冻结不可逆，故先写对象后冻结）。
func PutBaselineName(ctx context.Context, obj repository.ObjectPutter, bucket string, id uint64, name string) error {
	if name == "" {
		return fmt.Errorf("store: baseline name of %d is empty", id)
	}
	b, err := json.Marshal(BaselineName{ExperimentID: FormatID(id), Name: name})
	if err != nil {
		return fmt.Errorf("store: baseline name: %w", err)
	}
	key := BaselineNameKey(id)
	if err := obj.PutObject(ctx, bucket, key, b, manifestContentType); err != nil {
		return fmt.Errorf("store: upload %s: %w", key, err)
	}
	return nil
}

// CheckBaselineName 要求 name 与实验已登记的基线名一致（pointer-only 只能补写同名指针，不能给已冻结实验另起名字）。
// 对象读不到、解析失败、experiment_id 不符或登记名为空都返回 ErrBaselineName。
func CheckBaselineName(ctx context.Context, obj repository.ObjectGetter, bucket string, id uint64, name string) error {
	key := BaselineNameKey(id)
	raw, err := obj.GetObject(ctx, bucket, key)
	if err != nil {
		return fmt.Errorf("%w: read %s/%s: %w", ErrBaselineName, bucket, key, err)
	}
	var got BaselineName
	if err := json.Unmarshal(raw, &got); err != nil {
		return fmt.Errorf("%w: parse %s/%s: %w", ErrBaselineName, bucket, key, err)
	}
	switch {
	case got.ExperimentID != FormatID(id) || got.Name == "":
		return fmt.Errorf("%w: %s/%s = %+v, want experiment_id %d with a name", ErrBaselineName, bucket, key, got, id)
	case got.Name != name:
		return fmt.Errorf("%w: exp %d is registered as %q, not %q", ErrBaselineName, id, got.Name, name)
	}
	return nil
}
