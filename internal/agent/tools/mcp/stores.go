package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"time"

	"agri-steward/internal/model"
)

// IdemStore 是 idem:tool 去重占位（母文档 Redis 键约定 5），只用基本类型；
// 比较后改写 / 删除须原子（Redis 实现用 Lua 比对整值）。实现：repository.RedisIdem。
type IdemStore interface {
	// SetNX 在键不存在时写入，返回是否写入。
	SetNX(ctx context.Context, key, value string, ttl time.Duration) (bool, error)
	// Get 读值，键不存在返回 false。
	Get(ctx context.Context, key string) (string, bool, error)
	// Replace 仅当当前值等于 expect 时改写为 value 并重设 TTL，返回是否改写。
	Replace(ctx context.Context, key, expect, value string, ttl time.Duration) (bool, error)
	// DeleteIf 仅当当前值等于 expect 时删除，返回是否删除。
	DeleteIf(ctx context.Context, key, expect string) (bool, error)
}

// Cache 是 cache:mcp 只读结果缓存。实现：repository.RedisCache。
type Cache interface {
	Get(ctx context.Context, key string) ([]byte, bool, error)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
}

// FieldScope 校验地块属于 scope 的租户 / 农场（租户隔离红线：入参 field_id 越权即 REJECTED，不调 Server）。
// 实现：repository.FieldScope。
type FieldScope interface {
	Owns(ctx context.Context, scope model.Scope, fieldID uint64) (bool, error)
}

// Keys 生成 Redis 实际键（带 as:{env}: 前缀）；rediskey.Keys 满足此接口。
type Keys interface {
	IdemTool(callKey string) string
	CacheMCP(tool, argsDigest string) string
}

// callKeyLen 是 call_key 的长度（tool_call.call_key char(32)）。
const callKeyLen = 32

// Canonical 返回入参的规范形式：JSON 对象键排序、去空白、数字原样保留；
// 非法 JSON 原样返回（此类调用会被 policy.Before 拒绝，摘要只用于落行）。空入参视为 {}。
func Canonical(args json.RawMessage) []byte {
	if len(bytes.TrimSpace(args)) == 0 {
		return []byte(`{}`)
	}
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return append([]byte(nil), args...)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return append([]byte(nil), args...)
	}
	return bytes.TrimRight(buf.Bytes(), "\n")
}

// ArgsDigest 是 tool_call.args_digest：hex(sha256(canonical(args)))，64 位。
func ArgsDigest(canonical []byte) string {
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}

// CallKey 按 P0 §6.3 流程 2 计算：hex(sha256(run_id|step_id|tool|canonical(args)|call_seq|epoch))[:32]；
// epoch 由调用方按是否时效工具传入（非时效恒 0）。
func CallKey(runID uint64, stepID, tool string, canonical []byte, callSeq, epoch int) string {
	b := make([]byte, 0, len(canonical)+len(stepID)+len(tool)+64)
	b = strconv.AppendUint(b, runID, 10)
	b = append(b, '|')
	b = append(b, stepID...)
	b = append(b, '|')
	b = append(b, tool...)
	b = append(b, '|')
	b = append(b, canonical...)
	b = append(b, '|')
	b = strconv.AppendInt(b, int64(callSeq), 10)
	b = append(b, '|')
	b = strconv.AppendInt(b, int64(epoch), 10)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:callKeyLen]
}
