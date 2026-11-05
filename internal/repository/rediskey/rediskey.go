// Package rediskey 统一拼接 Redis 实际键：as:{env}: + 各阶段文档键名（母文档 Redis 键约定 1）。
// 评测 trial 的 env 取 eval-t{n}，trial 结束按 Prefix 删除。
package rediskey

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"time"
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

// IdemApprove 是 idem:approve:{request_id}（P0 L6，PENDING:{token} 10s / DONE:{decision_id} 24h）。
func (k Keys) IdemApprove(requestID string) string { return k.prefix + "idem:approve:" + requestID }

// CacheMCP 是 cache:mcp:{tool}:{tenant_id}:{farm_id}:{args_digest}（P0 L2 TTL：weather 10min / crop 24h）。
// P1 L1-B 按农场隔离；farm_id 只在租户内唯一（uk_farm(tenant_id, farm_id)），故带 tenant_id（偏离 P1 §6.2 键表，见 dev-log）。
func (k Keys) CacheMCP(tool string, tenantID, farmID uint64, argsDigest string) string {
	return k.prefix + "cache:mcp:" + tool + ":" + strconv.FormatUint(tenantID, 10) + ":" +
		strconv.FormatUint(farmID, 10) + ":" + argsDigest
}

// LockRun 是 lock:run:{run_id}（P0 L4，TTL 30s，执行期间按 ttl/3 续期）。
func (k Keys) LockRun(runID uint64) string {
	return k.prefix + "lock:run:" + strconv.FormatUint(runID, 10)
}

// RateFarmAsk 是 rl:farm:{farm_id}:ask（P0 L4，1 分钟固定窗口计数）。
func (k Keys) RateFarmAsk(farmID uint64) string {
	return k.prefix + "rl:farm:" + strconv.FormatUint(farmID, 10) + ":ask"
}

// SSERun 是 SSE 频道 sse:run:{run_id}（P0 L7：run_event 提交后 PUBLISH）。
func (k Keys) SSERun(runID uint64) string {
	return k.prefix + "sse:run:" + strconv.FormatUint(runID, 10)
}

// IdemDispatchTTL 是 idem:dispatch 键的 TTL（P1 §6.3：7 天，覆盖回执迟到与人工对账窗口）。
const IdemDispatchTTL = 7 * 24 * time.Hour

// IdemDispatch 是 idem:dispatch:{order_id}:{ticket_id}:{attempt}（P1 L2，TTL IdemDispatchTTL）：
// 同一张票据批准的同一次尝试只调用一次设备 Adapter。
func (k Keys) IdemDispatch(orderID, ticketID uint64, attempt int) string {
	return k.prefix + "idem:dispatch:" + strconv.FormatUint(orderID, 10) + ":" + strconv.FormatUint(ticketID, 10) + ":" +
		strconv.Itoa(attempt)
}

// Raw 拼接其他层文档中的键名（如 cache:mcp:...），各层落地时再补专用方法。
func (k Keys) Raw(name string) string { return k.prefix + name }

// IdemReceiptTTL 是 idem:receipt 键的 TTL（P1 §6.5：7 天）。
const IdemReceiptTTL = 7 * 24 * time.Hour

// IdemReceipt 是 idem:receipt:{receipt_id}（P1 L4，TTL IdemReceiptTTL）：回执事务提交后写入，重放快速返回；丢失由 uk_receipt 兜底。
func (k Keys) IdemReceipt(receiptID string) string { return k.prefix + "idem:receipt:" + receiptID }

// ScanReceiptDue 是回执超时扫描的任务名（lock:scan:receipt_due，P1 §6.5）。
const ScanReceiptDue = "receipt_due"

// LockP2JobTTL 是 lock:p2job 键的 TTL（P2 §6.4 Redis 键表：10min，执行中续期）。
const LockP2JobTTL = 10 * time.Minute

// LockP2Job 是 lock:p2job:{job}（P2 第 3 批接缝）：P2 批任务单实例互斥，防止手动补跑与定时任务并发（L3 / L4 / L5 共用）。
func (k Keys) LockP2Job(job string) string { return k.prefix + "lock:p2job:" + job }

// RlTokenTTL 是 rl:judge:{tier} 令牌桶键的 TTL（P2 §6.4 Redis 键表：2min，按分钟滚动）。
const RlTokenTTL = 2 * time.Minute

// RlJudge 是 rl:judge:{tier}（P2 L3）：线上抽样 judge 的 QPM 上限（令牌桶）；judge 同时从生产 rl:model:{tier} 取令牌。
func (k Keys) RlJudge(tier string) string { return k.prefix + "rl:judge:" + tier }

// ReviewEnqueueCursorTTL 是 cursor:review_enqueue 键的 TTL（P2-L2b：7 天，远长于入队周期；过期只让该农场下一轮从头扫，
// 评测库按 ref_key 去重，不会重复派单）。
const ReviewEnqueueCursorTTL = 7 * 24 * time.Hour

// ReviewEnqueueCursor 是 cursor:review_enqueue:{tenant_id}:{farm_id}（P2-L2b，TTL ReviewEnqueueCursorTTL）：
// 线上 judge 专家队列入队单轮读满页数上限时记下最后一个 online_judge_result.id，下一轮从这里续扫；扫到末尾即删除。
func (k Keys) ReviewEnqueueCursor(tenantID, farmID uint64) string {
	return k.prefix + "cursor:review_enqueue:" + strconv.FormatUint(tenantID, 10) + ":" + strconv.FormatUint(farmID, 10)
}
