package repository

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"

	"github.com/minio/minio-go/v7"
	"gorm.io/gorm"

	"agri-steward/internal/model"
)

// ObjectPutter 是 RawStore 需要的对象存储能力（minio-go 适配见 MinIOPutter）；换 OSS 只换 endpoint 与密钥。
type ObjectPutter interface {
	PutObject(ctx context.Context, bucket, key string, body []byte, contentType string) error
}

// MinIOPutter 用 minio-go 实现 ObjectPutter。
type MinIOPutter struct {
	Client *minio.Client
}

// PutObject 实现 ObjectPutter。
func (p MinIOPutter) PutObject(ctx context.Context, bucket, key string, body []byte, contentType string) error {
	_, err := p.Client.PutObject(ctx, bucket, key, bytes.NewReader(body), int64(len(body)),
		minio.PutObjectOptions{ContentType: contentType})
	if err != nil {
		return fmt.Errorf("repository: minio put %s/%s: %w", bucket, key, err)
	}
	return nil
}

// RawStore 先写对象存储，再登记 raw_object（agent.RawStore 的实现）。
type RawStore struct {
	db     *gorm.DB
	obj    ObjectPutter
	bucket string
}

// NewRawStore 构造 RawStore；bucket 为 agri-steward-raw。
func NewRawStore(db *gorm.DB, obj ObjectPutter, bucket string) *RawStore {
	return &RawStore{db: db, obj: obj, bucket: bucket}
}

// rawNamePattern 限定 key 的 {id} 段：不含 '/'，避免越出 raw/ 目录结构。
var rawNamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// ErrBadRawPut 表示 RawPut 参数不合法。
var ErrBadRawPut = errors.New("repository: bad raw put")

// RawObjectKey 按母文档 §4.6.2-8 生成 key：raw/{tenant}/{farm}/{yyyy}/{mm}/{dd}/{run_id 或 0}/{kind}/{id}。
func RawObjectKey(p model.RawPut) (string, error) {
	if !p.Kind.Valid() {
		return "", fmt.Errorf("%w: kind %q", ErrBadRawPut, p.Kind)
	}
	if !rawNamePattern.MatchString(p.Name) {
		return "", fmt.Errorf("%w: name %q", ErrBadRawPut, p.Name)
	}
	if p.At.IsZero() {
		return "", fmt.Errorf("%w: At is zero", ErrBadRawPut)
	}
	at := p.At.UTC()
	return fmt.Sprintf("raw/%d/%d/%04d/%02d/%02d/%s/%s/%s", p.TenantID, p.FarmID, at.Year(), int(at.Month()), at.Day(),
		strconv.FormatUint(p.RunID, 10), p.Kind, p.Name), nil
}

// Digest 返回 body 的 sha256 十六进制串（raw_object.sha256 与审批快照 snapshot_digest 同一口径）。
func Digest(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// Put 写对象后登记 raw_object；同一 key 再次写入会覆盖对象本体并返回已有行 id（uk_key 幂等）。
// 不同内容不得复用同一 key：需要区分版本的调用方把内容摘要放进 Name（如审批快照 ticket-{order}-{seq}-{digest 前缀}）。
func (s *RawStore) Put(ctx context.Context, p model.RawPut) (model.RawObject, error) {
	key, err := RawObjectKey(p)
	if err != nil {
		return model.RawObject{}, err
	}
	if err := s.obj.PutObject(ctx, s.bucket, key, p.Body, p.ContentType); err != nil {
		return model.RawObject{}, err
	}
	obj := model.RawObject{TenantID: p.TenantID, FarmID: p.FarmID, Kind: p.Kind, ObjectKey: key,
		SHA256: Digest(p.Body), SizeBytes: int64(len(p.Body)), CreatedAt: p.At.UTC()}
	if p.RunID != 0 {
		runID := p.RunID
		obj.RunID = &runID
	}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		res, err := execInTx(ctx, tx,
			`INSERT INTO raw_object (tenant_id, farm_id, run_id, kind, object_key, sha256, size_bytes, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?) AS new
  ON DUPLICATE KEY UPDATE id = LAST_INSERT_ID(id), sha256 = new.sha256, size_bytes = new.size_bytes`,
			obj.TenantID, obj.FarmID, obj.RunID, string(obj.Kind), obj.ObjectKey, obj.SHA256, obj.SizeBytes, obj.CreatedAt)
		if err != nil {
			return fmt.Errorf("repository: insert raw_object %s: %w", key, err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("repository: raw_object id: %w", err)
		}
		obj.ID = uint64(id)
		return nil
	})
	if err != nil {
		return model.RawObject{}, err
	}
	return obj, nil
}
