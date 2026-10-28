package kit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"agri-steward/internal/model"
)

// FieldDirectory 把 field_id（field_profile.id）翻成外部地块编码（ext_field_code），
// mock 数据按编码组织，与种子和评测样本的 field-00x 一致。
// 生产实现：repository.FieldDirectory（查 field_profile）；单测用 StaticFields。
type FieldDirectory interface {
	FieldCode(ctx context.Context, fieldID uint64) (string, error)
}

// StaticFields 是内存版 FieldDirectory（单测与本地联调）。
type StaticFields map[uint64]string

// FieldCode 实现 FieldDirectory；未登记返回 model.ErrNotFound。
func (m StaticFields) FieldCode(_ context.Context, fieldID uint64) (string, error) {
	if code, ok := m[fieldID]; ok {
		return code, nil
	}
	return "", fmt.Errorf("field %d: %w", fieldID, model.ErrNotFound)
}

// ResolveField 校验 field_id 并查出地块编码：地块不存在按 EMPTY（无数据）返回，其余错误原样包装。
func ResolveField(ctx context.Context, dir FieldDirectory, a FieldArgs) (string, error) {
	if err := a.Require(); err != nil {
		return "", err
	}
	code, err := dir.FieldCode(ctx, a.FieldID)
	if errors.Is(err, model.ErrNotFound) {
		return "", Empty("field %d not found", a.FieldID)
	}
	if err != nil {
		return "", fmt.Errorf("resolve field %d: %w", a.FieldID, err)
	}
	return code, nil
}

// FieldScoped 是带 field_id 的入参结构体指针（嵌入 FieldArgs 即满足）。
type FieldScoped interface{ Field() FieldArgs }

// Field 实现 FieldScoped。
func (a FieldArgs) Field() FieldArgs { return a }

// DecodeField 严格解码入参并解析地块编码。
func DecodeField(ctx context.Context, dir FieldDirectory, args json.RawMessage, dst FieldScoped) (string, error) {
	if err := DecodeArgs(args, dst); err != nil {
		return "", err
	}
	return ResolveField(ctx, dir, dst.Field())
}

// Lookup 按地块编码取 mock 数据，缺失按 EMPTY 返回。
func Lookup[T any](m map[string]T, code, what string) (T, error) {
	v, ok := m[code]
	if !ok {
		var zero T
		return zero, Empty("no %s for %s", what, code)
	}
	return v, nil
}

// Day 是一天（UTC 零点）的长度，按天偏移的 mock 数据共用。
const Day = 24 * time.Hour

// DayOf 截到 UTC 零点。
func DayOf(t time.Time) time.Time { return t.UTC().Truncate(Day) }
