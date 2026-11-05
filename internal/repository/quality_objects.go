package repository

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/minio/minio-go/v7"

	"agri-steward/internal/model"
)

// QualityObjects 读写 eval 桶里 L3 的对象（nightly/latest.json、{experiment_id}/metrics.json、report/daily/*.html）。
// 对象不存在时 GetObject 返回 model.ErrNotFound（日报离线栏据此显示「无数据」，其余错误让任务下个周期重试）。
type QualityObjects struct {
	Client *minio.Client
}

// GetObject 读一个对象。
func (o QualityObjects) GetObject(ctx context.Context, bucket, key string) ([]byte, error) {
	if _, err := o.Client.StatObject(ctx, bucket, key, minio.StatObjectOptions{}); err != nil {
		if minio.ToErrorResponse(err).Code == "NoSuchKey" {
			return nil, fmt.Errorf("repository: %s/%s: %w", bucket, key, model.ErrNotFound)
		}
		return nil, fmt.Errorf("repository: minio stat %s/%s: %w", bucket, key, err)
	}
	return MinIOGetter(o).GetObject(ctx, bucket, key)
}

// PutObject 写一个对象。
func (o QualityObjects) PutObject(ctx context.Context, bucket, key string, body []byte, contentType string) error {
	return MinIOPutter(o).PutObject(ctx, bucket, key, body, contentType)
}

// ErrObjectExists 表示条件写（PutObjectIfAbsent）时对象已存在。
var ErrObjectExists = errors.New("repository: object already exists")

// PutObjectIfAbsent 仅在对象不存在时写入（If-None-Match: *，MinIO 支持）：已存在返回 ErrObjectExists，不覆盖。
// 并发写同一键只有一个成功（P2-L2b BACKFLOW_EXPECT 结论）。
func (o QualityObjects) PutObjectIfAbsent(ctx context.Context, bucket, key string, body []byte, contentType string) error {
	opts := minio.PutObjectOptions{ContentType: contentType}
	opts.SetMatchETagExcept("*")
	if _, err := o.Client.PutObject(ctx, bucket, key, bytes.NewReader(body), int64(len(body)), opts); err != nil {
		if minio.ToErrorResponse(err).Code == minio.PreconditionFailed {
			return fmt.Errorf("repository: %s/%s: %w", bucket, key, ErrObjectExists)
		}
		return fmt.Errorf("repository: minio put-if-absent %s/%s: %w", bucket, key, err)
	}
	return nil
}
