package repository

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/minio/minio-go/v7"
)

// P1 L7 对象存储能力：删对象（保留期清理、导出命中 pii_clean）与锚点 bucket（object lock COMPLIANCE）。

// ObjectRemover 删除对象；对象不存在视为成功（清理可重试）。
type ObjectRemover interface {
	RemoveObject(ctx context.Context, bucket, key string) error
}

// MinIORemover 用 minio-go 实现 ObjectRemover。
type MinIORemover struct{ Client *minio.Client }

// RemoveObject 实现 ObjectRemover（S3 删除不存在的 key 也返回成功）。
func (r MinIORemover) RemoveObject(ctx context.Context, bucket, key string) error {
	if err := r.Client.RemoveObject(ctx, bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("repository: minio remove %s/%s: %w", bucket, key, err)
	}
	return nil
}

// ErrAnchorNotFound 表示锚点 bucket 中没有该对象。
var ErrAnchorNotFound = errors.New("repository: anchor object not found")

// MinIOAnchorStore 读写锚点 bucket（agri-steward-audit-anchor）；对象保留期由 bucket 的缺省 object lock 规则施加。
type MinIOAnchorStore struct {
	Client *minio.Client
	Bucket string
}

// PutAnchor 写一个锚点对象。
func (s MinIOAnchorStore) PutAnchor(ctx context.Context, key string, body []byte) error {
	_, err := s.Client.PutObject(ctx, s.Bucket, key, bytes.NewReader(body), int64(len(body)),
		minio.PutObjectOptions{ContentType: "application/json"})
	if err != nil {
		return fmt.Errorf("repository: minio put anchor %s/%s: %w", s.Bucket, key, err)
	}
	return nil
}

// AnchorObject 是锚点对象的最早版本内容；Versions 是该 key 的版本数（含删除标记），>1 表示被重写或删除过。
type AnchorObject struct {
	Body     []byte
	Versions int
}

// GetAnchor 列出 key 的全部版本，读最早写入的版本（object lock 下重写只会新增版本，最早版本才是原始锚点）；
// 不存在返回 ErrAnchorNotFound。
func (s MinIOAnchorStore) GetAnchor(ctx context.Context, key string) (AnchorObject, error) {
	ctx, cancel := context.WithCancel(ctx) // 提前返回时停止列举协程
	defer cancel()
	var first minio.ObjectInfo
	live, versions := 0, 0
	for o := range s.Client.ListObjects(ctx, s.Bucket, minio.ListObjectsOptions{Prefix: key, WithVersions: true}) {
		if o.Err != nil {
			return AnchorObject{}, fmt.Errorf("repository: minio list anchor versions %s/%s: %w", s.Bucket, key, o.Err)
		}
		if o.Key != key {
			continue
		}
		versions++
		if o.IsDeleteMarker {
			continue
		}
		live++
		// 列举按新到旧：时间相同时取后列出的（更早写入）
		if live == 1 || !o.LastModified.After(first.LastModified) {
			first = o
		}
	}
	if live == 0 {
		return AnchorObject{}, fmt.Errorf("%w: %s/%s", ErrAnchorNotFound, s.Bucket, key)
	}
	opts := minio.GetObjectOptions{}
	if first.VersionID != "" && first.VersionID != "null" { // 未开版本的 bucket 版本号为 "null"
		opts.VersionID = first.VersionID
	}
	obj, err := s.Client.GetObject(ctx, s.Bucket, key, opts)
	if err != nil {
		return AnchorObject{}, fmt.Errorf("repository: minio get anchor %s/%s: %w", s.Bucket, key, err)
	}
	body, readErr := io.ReadAll(obj)
	if err := errors.Join(readErr, obj.Close()); err != nil {
		if minio.ToErrorResponse(readErr).Code == "NoSuchKey" {
			return AnchorObject{}, fmt.Errorf("%w: %s/%s", ErrAnchorNotFound, s.Bucket, key)
		}
		return AnchorObject{}, fmt.Errorf("repository: minio read anchor %s/%s: %w", s.Bucket, key, err)
	}
	return AnchorObject{Body: body, Versions: versions}, nil
}

// EnsureAnchorBucket 创建锚点 bucket 并开启 object lock（COMPLIANCE，保留 days 天）；已存在时重设缺省保留规则。
// MinIO 不支持 object lock（单盘 fs 模式等）或已有 bucket 建桶时未开 object lock，返回明确错误，不降级为普通 bucket。
func EnsureAnchorBucket(ctx context.Context, c *minio.Client, bucket string, days int) error {
	if days <= 0 {
		return fmt.Errorf("repository: anchor bucket %s: lock days must be > 0, got %d", bucket, days)
	}
	ok, err := c.BucketExists(ctx, bucket)
	if err != nil {
		return fmt.Errorf("repository: minio %s: check bucket %s: %w", c.EndpointURL().Host, bucket, err)
	}
	if !ok {
		if err := c.MakeBucket(ctx, bucket, minio.MakeBucketOptions{ObjectLocking: true}); err != nil {
			return fmt.Errorf("repository: make anchor bucket %s with object lock failed (MinIO 须支持 object lock): %w", bucket, err)
		}
	}
	mode, validity, unit := minio.Compliance, uint(days), minio.Days
	if err := c.SetObjectLockConfig(ctx, bucket, &mode, &validity, &unit); err != nil {
		return fmt.Errorf("repository: anchor bucket %s: set object lock COMPLIANCE %d days failed "+
			"(MinIO 不支持 object lock，或该 bucket 建桶时未开启 object lock，须删桶重建): %w", bucket, days, err)
	}
	return nil
}
