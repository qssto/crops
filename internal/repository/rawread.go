package repository

import (
	"context"
	"fmt"

	"gorm.io/gorm"

	"agri-steward/internal/model"
)

// RawReader 按 raw_object id（限定租户 / 农场）读回对象本体（mcp.RawReader 的实现：去重命中时取回未脱敏的原始结果）。
type RawReader struct {
	DB     *gorm.DB
	Obj    ObjectGetter
	Bucket string // 与 RawStore 同一 bucket（agri-steward-raw）
}

// Get 取 object_key 后读对象；行不存在返回 model.ErrNotFound。
func (r RawReader) Get(ctx context.Context, scope model.Scope, rawObjectID uint64) ([]byte, error) {
	if r.DB == nil || r.Obj == nil {
		return nil, fmt.Errorf("repository: raw_object %d: RawReader not configured", rawObjectID)
	}
	var key string
	res := r.DB.WithContext(ctx).Raw(`SELECT object_key FROM raw_object WHERE id=? AND tenant_id=? AND farm_id=?`,
		rawObjectID, scope.TenantID, scope.FarmID).Scan(&key)
	if res.Error != nil {
		return nil, fmt.Errorf("repository: get raw_object %d: %w", rawObjectID, res.Error)
	}
	if res.RowsAffected == 0 {
		return nil, fmt.Errorf("repository: raw_object %d: %w", rawObjectID, model.ErrNotFound)
	}
	body, err := r.Obj.GetObject(ctx, r.Bucket, key)
	if err != nil {
		return nil, fmt.Errorf("repository: read raw_object %d: %w", rawObjectID, err)
	}
	return body, nil
}
