package service

import (
	"context"
	"time"

	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// NotifyOutboxWriter 是 notify.send 的事务内写入（P1 L5 / L6 共用接缝），repository.NotifyOutboxRepo 实现。
// dedupKey 用 model.NotifyDedupKey；payload 以 model.NotifyEnvelope 为公共字段。返回是否新插入。
type NotifyOutboxWriter interface {
	InsertTx(ctx context.Context, tx *repository.DB, ref model.OrderRef, dedupKey string, payload []byte,
		now time.Time) (bool, error)
}
