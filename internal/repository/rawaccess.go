package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/minio/minio-go/v7"
	"gorm.io/gorm"

	"agri-steward/internal/model"
)

// Presigner 签发对象的限时下载 URL（minio-go 适配见 MinIOPresigner）。
type Presigner interface {
	PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error)
}

// MinIOPresigner 用 minio-go PresignedGetObject 实现 Presigner；Bucket 与 RawStore 同一个（agri-steward-raw）。
type MinIOPresigner struct {
	Client *minio.Client
	Bucket string
}

// PresignGet 实现 Presigner。签名在本地计算，不访问 MinIO。
func (p MinIOPresigner) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	u, err := p.Client.PresignedGetObject(ctx, p.Bucket, key, ttl, nil)
	if err != nil {
		return "", fmt.Errorf("repository: presign %s/%s: %w", p.Bucket, key, err)
	}
	return u.String(), nil
}

// RawObjectRow 是 raw_object 中读原文授权需要的列；RunID 为 0 表示不属于任何 run。
type RawObjectRow struct {
	ID        uint64
	RunID     uint64
	Kind      model.RawKind
	ObjectKey string
}

// RawAccessRepo 是读原文（RAW_READ）需要的仓储能力：按 scope 查 raw_object + 写审计。
type RawAccessRepo struct {
	DB *gorm.DB
}

// InTx 开事务；提交后执行经 AppendTx 登记的 after-commit 回调。
func (r RawAccessRepo) InTx(ctx context.Context, fn func(tx *DB) error) error {
	return inTx(ctx, r.DB, fn)
}

// RawTx 按 id 取 raw_object（限定租户 / 农场）；不存在或不属于该 scope 均返回 model.ErrNotFound。
func (r RawAccessRepo) RawTx(ctx context.Context, tx *DB, scope model.Scope, rawID uint64) (RawObjectRow, error) {
	var row struct {
		ID        uint64
		RunID     *uint64
		Kind      string
		ObjectKey string
	}
	res := tx.WithContext(ctx).Raw(
		`SELECT id, run_id, kind, object_key FROM raw_object WHERE id=? AND tenant_id=? AND farm_id=?`,
		rawID, scope.TenantID, scope.FarmID).Scan(&row)
	if res.Error != nil {
		return RawObjectRow{}, fmt.Errorf("repository: get raw_object %d: %w", rawID, res.Error)
	}
	if res.RowsAffected == 0 {
		return RawObjectRow{}, fmt.Errorf("repository: raw_object %d: %w", rawID, model.ErrNotFound)
	}
	out := RawObjectRow{ID: row.ID, Kind: model.RawKind(row.Kind), ObjectKey: row.ObjectKey}
	if row.RunID != nil {
		out.RunID = *row.RunID
	}
	return out, nil
}

// AuditTx 同事务写一条审计（RAW_READ/OK 与签发同事务）。
func (r RawAccessRepo) AuditTx(ctx context.Context, tx *DB, rec model.AuditRecord) error {
	return AuditRepo{}.AppendTx(ctx, tx, rec)
}

// Audit 独立短事务写一条审计（RAW_READ/DENIED）。
func (r RawAccessRepo) Audit(ctx context.Context, rec model.AuditRecord) error {
	return AuditRepo{DB: r.DB}.Append(ctx, rec)
}
