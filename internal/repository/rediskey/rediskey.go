// Package rediskey 统一拼接 Redis 实际键：as:{env}: + 各阶段文档键名（母文档 Redis 键约定 1）。
// 评测 trial 的 env 取 eval-t{n}，trial 结束按 Prefix 删除。
package rediskey

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
)

// envPattern 限定 env 只含小写字母、数字与连字符，避免拼出含 ':' 或通配符的前缀。
var envPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// ErrBadEnv 表示 env 不合法。
var ErrBadEnv = errors.New("rediskey: env must match ^[a-z0-9][a-z0-9-]{0,31}$")

// Keys 按 env 生成键。零值不可用，须经 New 构造。
type Keys struct {
	prefix string
}

// New 构造 env 对应的 Keys。
func New(env string) (Keys, error) {
	if !envPattern.MatchString(env) {
		return Keys{}, fmt.Errorf("%w: %q", ErrBadEnv, env)
	}
	return Keys{prefix: "as:" + env + ":"}, nil
}

// EvalTrialEnv 返回第 n 个 trial schema 对应的 env：eval-t{n}。
func EvalTrialEnv(n int) string { return "eval-t" + strconv.Itoa(n) }

// Prefix 返回 as:{env}:，用于按前缀清理。
func (k Keys) Prefix() string { return k.prefix }

// LockOrder 是 lock:order:{order_id}（P0 L1，TTL 10s）。
func (k Keys) LockOrder(orderID uint64) string {
	return k.prefix + "lock:order:" + strconv.FormatUint(orderID, 10)
}

// LockScan 是 lock:scan:{task}（P0 L1，TTL 60s）。
func (k Keys) LockScan(task string) string { return k.prefix + "lock:scan:" + task }

// IdemTool 是 idem:tool:{call_key}（P0 L2，PENDING 9s / DONE 24h）。
func (k Keys) IdemTool(callKey string) string { return k.prefix + "idem:tool:" + callKey }

// CacheMCP 是 cache:mcp:{tool}:{args_digest}（P0 L2，weather 10min / crop 24h）。
func (k Keys) CacheMCP(tool, argsDigest string) string {
	return k.prefix + "cache:mcp:" + tool + ":" + argsDigest
}

// LockRun 是 lock:run:{run_id}（P0 L4，TTL 30s，执行期间按 ttl/3 续期）。
func (k Keys) LockRun(runID uint64) string {
	return k.prefix + "lock:run:" + strconv.FormatUint(runID, 10)
}

// RateFarmAsk 是 rl:farm:{farm_id}:ask（P0 L4，1 分钟固定窗口计数）。
func (k Keys) RateFarmAsk(farmID uint64) string {
	return k.prefix + "rl:farm:" + strconv.FormatUint(farmID, 10) + ":ask"
}

// Raw 拼接其他层文档中的键名（如 cache:mcp:...），各层落地时再补专用方法。
func (k Keys) Raw(name string) string { return k.prefix + name }
