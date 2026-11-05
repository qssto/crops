package repository

import (
	"context"
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
