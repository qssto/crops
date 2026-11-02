package seed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// flagCommentMaxRunes 与 feature_flag.comment varchar(256) 一致。
const flagCommentMaxRunes = 256

// flagEntry 是种子文件 flags: 的一行（P1-L0）；value 为 JSON 字符串。
type flagEntry struct {
	FlagKey  string `yaml:"flag_key"`
	TenantID uint64 `yaml:"tenant_id"`
	FarmID   uint64 `yaml:"farm_id"`
	Value    string `yaml:"value"`
	Comment  string `yaml:"comment"`
}

type flagLoc struct {
	tenantID, farmID uint64
	key              model.FlagKey
}

// loadFlags 按注册表校验 key / 作用域 / 形态，值规整后返回；同一 (tenant, farm, key) 只能出现一次。
func loadFlags(entries []flagEntry) ([]model.FlagWrite, error) {
	reg := model.DefaultFlagRegistry()
	seen := map[flagLoc]bool{}
	var out []model.FlagWrite
	var errs []error
	for i, e := range entries {
		key := model.FlagKey(e.FlagKey)
		spec, ok := reg[key]
		if !ok {
			errs = append(errs, fmt.Errorf("flags[%d]: unknown flag_key %q", i, e.FlagKey))
			continue
		}
		st, err := model.FlagScopeOf(e.TenantID, e.FarmID)
		if err != nil {
			errs = append(errs, fmt.Errorf("flags[%d]: %w", i, err))
			continue
		}
		v, err := model.ParseFlagValue(spec, st, json.RawMessage(e.Value))
		if err != nil {
			errs = append(errs, fmt.Errorf("flags[%d]: %w", i, err))
			continue
		}
		if key == model.FlagEnableRobotDispatch && st == model.FlagScopeFarm && v.FarmMode() == model.FlagModeOn {
			// 置开要过 enable_advice + agronomist 前置校验与 FLAG_CHANGE 审计，只能走管理接口。
			errs = append(errs, fmt.Errorf("flags[%d]: %s on must go through admin API", i, key))
			continue
		}
		if n := utf8.RuneCountInString(e.Comment); n == 0 || n > flagCommentMaxRunes {
			errs = append(errs, fmt.Errorf("flags[%d]: comment must be 1-%d runes", i, flagCommentMaxRunes))
			continue
		}
		loc := flagLoc{e.TenantID, e.FarmID, key}
		if seen[loc] {
			errs = append(errs, fmt.Errorf("flags[%d]: duplicate %s (%d,%d)", i, key, e.TenantID, e.FarmID))
			continue
		}
		seen[loc] = true
		canon, err := json.Marshal(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("flags[%d]: marshal value: %w", i, err))
			continue
		}
		out = append(out, model.FlagWrite{TenantID: e.TenantID, FarmID: e.FarmID, Key: key, ScopeType: st,
			Value: canon, Comment: e.Comment})
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return out, nil
}

// applyFlags 在种子事务内补齐缺失的 feature_flag 行；已有行不覆盖（重跑种子不会把管理员关掉的开关重新打开）。
func applyFlags(ctx context.Context, tx *repository.DB, flags []model.FlagWrite, now time.Time) error {
	repo := repository.FlagRepo{}
	for _, w := range flags {
		w.At = now
		if err := repo.InsertMissingTx(ctx, tx, w); err != nil {
			return fmt.Errorf("seed: %w", err)
		}
	}
	return nil
}
