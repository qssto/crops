package repository

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"

	"github.com/minio/minio-go/v7"
	"gorm.io/gorm"

	"agri-steward/internal/agent"
	"agri-steward/internal/clock"
	"agri-steward/internal/model"
)

// 检查点（P0 §6.7 流程 1）：只在中断时 Set，同一事务插入 run_checkpoint 并把 run RUNNING → INTERRUPTED(interrupt_type)；
// ≤ 256KB 写 payload，超限先经 RawStore 写 MinIO 再存 raw_object_id；每个 run 只保留最近 3 份。
const (
	// CheckpointInlineLimit 是 payload 直接入库的上限（字节）。
	CheckpointInlineLimit = 256 << 10
	// checkpointKeep 是每个 run 保留的检查点份数。
	checkpointKeep = 3
)

// ErrBadCheckpoint 表示检查点请求不合法：ctx 缺归属、checkpoint_id 与归属 run 不符或缺中断类型。
var ErrBadCheckpoint = errors.New("repository: bad checkpoint request")

// ObjectGetter 是读取 raw 对象的能力（minio-go 适配见 MinIOGetter）。
type ObjectGetter interface {
	GetObject(ctx context.Context, bucket, key string) ([]byte, error)
}

// MinIOGetter 用 minio-go 实现 ObjectGetter。
type MinIOGetter struct {
	Client *minio.Client
}

// GetObject 实现 ObjectGetter。
func (g MinIOGetter) GetObject(ctx context.Context, bucket, key string) ([]byte, error) {
	obj, err := g.Client.GetObject(ctx, bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("repository: minio get %s/%s: %w", bucket, key, err)
	}
	body, readErr := io.ReadAll(obj)
	closeErr := obj.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return nil, fmt.Errorf("repository: minio read %s/%s: %w", bucket, key, err)
	}
	return body, nil
}

// CheckpointRepo 实现 agent.CheckpointStore；归属（租户 / 农场 / 中断类型）取自 ctx 中的 agent.CheckpointTarget。
type CheckpointRepo struct {
	db     *gorm.DB
	clock  clock.Clock
	raw    agent.RawStore
	obj    ObjectGetter
	bucket string
}

var _ agent.CheckpointStore = (*CheckpointRepo)(nil)

// NewCheckpointRepo 构造 CheckpointRepo；raw / obj / bucket 用于超限 payload 的转存与读回（与 RawStore 同一 bucket）。
func NewCheckpointRepo(db *gorm.DB, c clock.Clock, raw agent.RawStore, obj ObjectGetter, bucket string) *CheckpointRepo {
	return &CheckpointRepo{db: db, clock: c, raw: raw, obj: obj, bucket: bucket}
}

// target 取 ctx 中的归属并校验与 checkpoint_id 一致。
func checkpointTarget(ctx context.Context, checkpointID string) (agent.CheckpointTarget, error) {
	t, ok := agent.CheckpointTargetFrom(ctx)
	if !ok || strconv.FormatUint(t.RunID, 10) != checkpointID {
		return agent.CheckpointTarget{}, fmt.Errorf("%w: checkpoint %s without matching target", ErrBadCheckpoint, checkpointID)
	}
	return t, nil
}

// Set 落检查点并置 run INTERRUPTED：run 非 RUNNING → model.ErrIllegalTransition，row_ver 冲突 → model.ErrConcurrentUpdate。
func (r *CheckpointRepo) Set(ctx context.Context, checkpointID string, payload []byte) error {
	t, err := checkpointTarget(ctx, checkpointID)
	if err != nil {
		return err
	}
	if t.Interrupt == "" {
		return fmt.Errorf("%w: checkpoint %s without interrupt type", ErrBadCheckpoint, checkpointID)
	}
	now := r.clock.Now()
	inline, rawID, err := r.spill(ctx, t, payload)
	if err != nil {
		return err
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var run struct {
			Status string
			RowVer int
		}
		res := tx.WithContext(ctx).Raw(`SELECT status, row_ver FROM agent_run WHERE id=? AND tenant_id=? AND farm_id=?`,
			t.RunID, t.Scope.TenantID, t.Scope.FarmID).Scan(&run)
		if res.Error != nil {
			return fmt.Errorf("repository: get agent_run %d: %w", t.RunID, res.Error)
		}
		if res.RowsAffected == 0 {
			return fmt.Errorf("repository: agent_run %d: %w", t.RunID, model.ErrNotFound)
		}
		if model.RunStatus(run.Status) != model.RunRunning {
			return fmt.Errorf("repository: interrupt run %d from %s: %w", t.RunID, run.Status, model.ErrIllegalTransition)
		}
		var seq int
		if err := tx.WithContext(ctx).Raw(
			`SELECT COALESCE(MAX(seq), 0) + 1 FROM run_checkpoint WHERE tenant_id=? AND farm_id=? AND run_id=?`,
			t.Scope.TenantID, t.Scope.FarmID, t.RunID).Scan(&seq).Error; err != nil {
			return fmt.Errorf("repository: next checkpoint seq run %d: %w", t.RunID, err)
		}
		if err := tx.WithContext(ctx).Exec(
			`INSERT INTO run_checkpoint (tenant_id, farm_id, run_id, seq, payload, raw_object_id, size_bytes, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			t.Scope.TenantID, t.Scope.FarmID, t.RunID, seq, inline, rawID, len(payload), now).Error; err != nil {
			return fmt.Errorf("repository: insert run_checkpoint run %d seq %d: %w", t.RunID, seq, err)
		}
		upd := tx.WithContext(ctx).Exec(
			`UPDATE agent_run SET status='INTERRUPTED', interrupt_type=?, row_ver=row_ver+1, updated_at=?
 WHERE id=? AND tenant_id=? AND farm_id=? AND status='RUNNING' AND row_ver=?`,
			string(t.Interrupt), now, t.RunID, t.Scope.TenantID, t.Scope.FarmID, run.RowVer)
		if upd.Error != nil {
			return fmt.Errorf("repository: interrupt agent_run %d: %w", t.RunID, upd.Error)
		}
		if upd.RowsAffected == 0 {
			return fmt.Errorf("repository: agent_run %d row_ver %d: %w", t.RunID, run.RowVer, model.ErrConcurrentUpdate)
		}
		// 只保留最近 checkpointKeep 份（文档未给 SQL，按 uk_run_seq 前缀删除）
		if err := tx.WithContext(ctx).Exec(
			`DELETE FROM run_checkpoint WHERE tenant_id=? AND farm_id=? AND run_id=? AND seq<=?`,
			t.Scope.TenantID, t.Scope.FarmID, t.RunID, seq-checkpointKeep).Error; err != nil {
			return fmt.Errorf("repository: prune run_checkpoint run %d: %w", t.RunID, err)
		}
		return nil
	})
}

// spill 决定 payload 入库方式：≤ 上限原样入库；超限先写 MinIO（raw_object.kind=CHECKPOINT），库内只存 raw_object_id。
func (r *CheckpointRepo) spill(ctx context.Context, t agent.CheckpointTarget, payload []byte) ([]byte, *uint64, error) {
	if len(payload) <= CheckpointInlineLimit {
		return payload, nil, nil
	}
	if r.raw == nil {
		return nil, nil, fmt.Errorf("repository: checkpoint run %d is %d bytes but no RawStore configured", t.RunID, len(payload))
	}
	now := r.clock.Now()
	obj, err := r.raw.Put(ctx, model.RawPut{TenantID: t.Scope.TenantID, FarmID: t.Scope.FarmID, RunID: t.RunID,
		Kind: model.RawCheckpoint, Name: fmt.Sprintf("ckpt-%d-%d", t.RunID, now.UnixMilli()), At: now,
		Body: payload, ContentType: "application/octet-stream"})
	if err != nil {
		return nil, nil, fmt.Errorf("repository: spill checkpoint run %d: %w", t.RunID, err)
	}
	return nil, &obj.ID, nil
}

// Get 读该 run 最近一份检查点；没有返回 false。超限的检查点从 MinIO 读回。
func (r *CheckpointRepo) Get(ctx context.Context, checkpointID string) ([]byte, bool, error) {
	t, err := checkpointTarget(ctx, checkpointID)
	if err != nil {
		return nil, false, err
	}
	var row struct {
		Payload     []byte
		RawObjectID *uint64
	}
	res := r.db.WithContext(ctx).Raw(
		`SELECT payload, raw_object_id FROM run_checkpoint WHERE tenant_id=? AND farm_id=? AND run_id=? ORDER BY seq DESC LIMIT 1`,
		t.Scope.TenantID, t.Scope.FarmID, t.RunID).Scan(&row)
	if res.Error != nil {
		return nil, false, fmt.Errorf("repository: get run_checkpoint run %d: %w", t.RunID, res.Error)
	}
	if res.RowsAffected == 0 {
		return nil, false, nil
	}
	if row.RawObjectID == nil {
		return row.Payload, true, nil
	}
	body, err := r.loadRaw(ctx, t.Scope, *row.RawObjectID)
	if err != nil {
		return nil, false, fmt.Errorf("repository: checkpoint run %d: %w", t.RunID, err)
	}
	return body, true, nil
}

// loadRaw 按 raw_object_id（限定租户 / 农场）取 object_key 后读对象。
func (r *CheckpointRepo) loadRaw(ctx context.Context, scope model.Scope, rawID uint64) ([]byte, error) {
	if r.obj == nil {
		return nil, fmt.Errorf("repository: raw_object %d: no ObjectGetter configured", rawID)
	}
	var key string
	res := r.db.WithContext(ctx).Raw(`SELECT object_key FROM raw_object WHERE id=? AND tenant_id=? AND farm_id=?`,
		rawID, scope.TenantID, scope.FarmID).Scan(&key)
	if res.Error != nil {
		return nil, fmt.Errorf("repository: get raw_object %d: %w", rawID, res.Error)
	}
	if res.RowsAffected == 0 {
		return nil, fmt.Errorf("repository: raw_object %d: %w", rawID, model.ErrNotFound)
	}
	return r.obj.GetObject(ctx, r.bucket, key)
}
