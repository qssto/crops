package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// P1 L7 每日锚点（§6.8 技术实现第 3 条）：worker 任务 audit_anchor 每日 00:10（北京时间）读全部 audit_chain_head，
// 写锚点 bucket 的 anchor/{yyyymmdd}.json，再 verify 自前一日锚点以来的增量，失败告警 audit_chain_broken（P0）。
// 幂等：anchor/{date}.json 已存在不重写（object lock 下重写只会多一个版本）；verify/{date}.json 是增量校验完成标记。

// ScanAuditAnchor 是锚点任务名（lock:scan:audit_anchor）。
const ScanAuditAnchor = "audit_anchor"

// anchorZone 是锚点日期口径（北京时间，农场缺省时区）。
var anchorZone = time.FixedZone("CST", 8*3600)

// anchorAfter 是每日锚定的最早时刻（00:10）。
const anchorAfter = 10 * time.Minute

// AnchorKey / AnchorVerifyKey 是锚点与增量校验标记的对象 key。
func AnchorKey(date string) string       { return "anchor/" + date + ".json" }
func AnchorVerifyKey(date string) string { return "verify/" + date + ".json" }

// AnchorDate 返回 t 在锚点口径下的日期 yyyymmdd。
func AnchorDate(t time.Time) string { return t.In(anchorZone).Format("20060102") }

// AnchorChain 是锚点中一条链的末端。
type AnchorChain struct {
	TenantID uint64 `json:"tenant_id"`
	FarmID   uint64 `json:"farm_id"`
	ChainSeq uint64 `json:"chain_seq"`
	HeadHash string `json:"head_hash"`
}

// Anchor 是 anchor/{yyyymmdd}.json：SHA256 = hex(sha256(RFC 8785 规范化的 chains 数组))。
type Anchor struct {
	Date       string        `json:"date"`
	AnchoredAt string        `json:"anchored_at"`
	Chains     []AnchorChain `json:"chains"`
	SHA256     string        `json:"sha256"`
}

// ErrAnchorDigest 表示锚点文件的整体 sha256 与 chains 不符。
var ErrAnchorDigest = errors.New("service: anchor sha256 mismatch")

func anchorDigest(chains []AnchorChain) (string, error) {
	if chains == nil {
		chains = []AnchorChain{}
	}
	b, err := json.Marshal(chains)
	if err != nil {
		return "", fmt.Errorf("service: marshal anchor chains: %w", err)
	}
	canon, err := repository.CanonicalJSON(b)
	if err != nil {
		return "", fmt.Errorf("service: canonicalize anchor chains: %w", err)
	}
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:]), nil
}

// BuildAnchor 由各链 head 构造锚点文件内容。
func BuildAnchor(date string, heads []repository.ChainHead, at time.Time) (Anchor, []byte, error) {
	a := Anchor{Date: date, AnchoredAt: at.UTC().Format("2006-01-02T15:04:05.000Z"), Chains: make([]AnchorChain, 0, len(heads))}
	for _, h := range heads {
		a.Chains = append(a.Chains, AnchorChain{TenantID: h.Key.TenantID, FarmID: h.Key.FarmID, ChainSeq: h.ChainSeq,
			HeadHash: h.HeadHash})
	}
	d, err := anchorDigest(a.Chains)
	if err != nil {
		return Anchor{}, nil, err
	}
	a.SHA256 = d
	body, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return Anchor{}, nil, fmt.Errorf("service: marshal anchor: %w", err)
	}
	return a, body, nil
}

// ParseAnchor 解析锚点文件并核对整体 sha256。
func ParseAnchor(b []byte) (Anchor, error) {
	var a Anchor
	if err := json.Unmarshal(b, &a); err != nil {
		return Anchor{}, fmt.Errorf("service: parse anchor: %w", err)
	}
	d, err := anchorDigest(a.Chains)
	if err != nil {
		return Anchor{}, err
	}
	if d != a.SHA256 {
		return a, fmt.Errorf("%w: file %s, computed %s", ErrAnchorDigest, a.SHA256, d)
	}
	return a, nil
}

// AnchorMismatchKind 是锚点核对发现的问题类型。
type AnchorMismatchKind string

const (
	AnchorHeadMissing AnchorMismatchKind = "HEAD_MISSING" // 锚点里的链在库中没有 head
	AnchorTruncated   AnchorMismatchKind = "TRUNCATED"    // 当前链短于锚点（截尾）或锚定行已不存在
	AnchorRecomputed  AnchorMismatchKind = "RECOMPUTED"   // 锚定位置的 row_hash 与锚点不符（截尾后重算 / 改写），或行内容重算与 row_hash 不符
)

// AnchorMismatch 是一条链与锚点的不一致。
type AnchorMismatch struct {
	Key      repository.ChainKey
	Kind     AnchorMismatchKind
	ChainSeq uint64
	Detail   string
}

func (m AnchorMismatch) String() string {
	return fmt.Sprintf("%s chain (%d,%d) chain_seq=%d %s", m.Kind, m.Key.TenantID, m.Key.FarmID, m.ChainSeq, m.Detail)
}

// CheckAnchor 比对锚点与当前链：锚定的 chain_seq 行必须仍在且 row_hash 等于锚定 head_hash。
// 「截尾后重算整条链」会让锚定位置的 row_hash 变化（RECOMPUTED）或该行消失（TRUNCATED），verify 本身发现不了。
func CheckAnchor(ctx context.Context, r ChainReader, a Anchor) ([]AnchorMismatch, error) {
	var out []AnchorMismatch
	for _, c := range a.Chains {
		k := repository.ChainKey{TenantID: c.TenantID, FarmID: c.FarmID}
		h, err := r.Head(ctx, k)
		if errors.Is(err, repository.ErrAuditChainHeadMissing) {
			out = append(out, AnchorMismatch{Key: k, Kind: AnchorHeadMissing, ChainSeq: c.ChainSeq})
			continue
		}
		if err != nil {
			return out, err
		}
		if c.ChainSeq == 0 {
			continue // 锚定时为空链
		}
		if h.ChainSeq < c.ChainSeq {
			out = append(out, AnchorMismatch{Key: k, Kind: AnchorTruncated, ChainSeq: c.ChainSeq,
				Detail: fmt.Sprintf("head chain_seq %d < anchored", h.ChainSeq)})
			continue
		}
		rows, err := r.Rows(ctx, k, c.ChainSeq, c.ChainSeq)
		if err != nil {
			return out, err
		}
		if len(rows) == 0 {
			out = append(out, AnchorMismatch{Key: k, Kind: AnchorTruncated, ChainSeq: c.ChainSeq, Detail: "anchored row missing"})
			continue
		}
		// 不信任存储的 row_hash：按行内容重算，重算值与存储值、锚点值三者须一致（改内容保留 row_hash 也能发现）
		got, err := repository.AuditRowHash(rows[0].PrevHash, rows[0])
		if err != nil && !errors.Is(err, repository.ErrNotCanonicalizable) {
			return out, fmt.Errorf("service: rehash anchored audit_log %d: %w", rows[0].ID, err)
		}
		switch {
		case err != nil || got != rows[0].RowHash:
			out = append(out, AnchorMismatch{Key: k, Kind: AnchorRecomputed, ChainSeq: c.ChainSeq,
				Detail: fmt.Sprintf("audit_log.id=%d content does not match stored row_hash", rows[0].ID)})
		case rows[0].RowHash != c.HeadHash:
			out = append(out, AnchorMismatch{Key: k, Kind: AnchorRecomputed, ChainSeq: c.ChainSeq,
				Detail: fmt.Sprintf("audit_log.id=%d row_hash differs from anchor", rows[0].ID)})
		}
	}
	return out, nil
}

// AnchorStore 读写锚点 bucket；GetAnchor 返回最早版本与版本数，不存在返回 repository.ErrAnchorNotFound。
// repository.MinIOAnchorStore 实现。
type AnchorStore interface {
	PutAnchor(ctx context.Context, key string, body []byte) error
	GetAnchor(ctx context.Context, key string) (repository.AnchorObject, error)
}

// AnchorChains 是锚点任务需要的链读能力：全部 head（系统级）+ 校验读。repository.AuditChainRepo 实现。
type AnchorChains interface {
	ChainReader
	Heads(ctx context.Context, sys repository.SystemScope) ([]repository.ChainHead, error)
}

// AnchorService 执行每日锚定与增量校验（由 Scanner 的 audit_anchor 任务驱动，持 lock:scan:audit_anchor）。
type AnchorService struct {
	Chains AnchorChains
	Store  AnchorStore
	Alerts model.AlertSink
	Logger *slog.Logger

	mu   sync.Mutex
	done string // 本进程已完成的锚点日期，避免每个扫描周期都读对象存储
}

// anchorVerifyMark 是 verify/{date}.json 的内容。
type anchorVerifyMark struct {
	Date       string   `json:"date"`
	PrevDate   string   `json:"prev_date"`
	VerifiedAt string   `json:"verified_at"`
	Chains     int      `json:"chains"`
	Rows       uint64   `json:"rows"`
	Problems   []string `json:"problems"`
}

// Tick 在 now（北京时间）过了当日 00:10 后锚定当日并校验前一日增量；已完成则直接返回。
func (s *AnchorService) Tick(ctx context.Context, sys repository.SystemScope, now time.Time) error {
	local := now.In(anchorZone)
	midnight := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, anchorZone)
	if local.Before(midnight.Add(anchorAfter)) {
		return nil
	}
	date := AnchorDate(now)
	s.mu.Lock()
	finished := s.done == date
	s.mu.Unlock()
	if finished {
		return nil
	}
	today, err := s.ensureAnchor(ctx, sys, date, now)
	if errors.Is(err, ErrAnchorDigest) { // 当日锚点被篡改：已告警，本进程当日不再重复
		s.markDone(date)
	}
	if err != nil {
		return err
	}
	if err := s.ensureVerified(ctx, today, AnchorDate(midnight.AddDate(0, 0, -1)), now); err != nil {
		return err
	}
	s.markDone(date)
	return nil
}

func (s *AnchorService) markDone(date string) {
	s.mu.Lock()
	s.done = date
	s.mu.Unlock()
}

// ensureAnchor 读当日锚点；不存在时读全部 head 写入。
func (s *AnchorService) ensureAnchor(ctx context.Context, sys repository.SystemScope, date string, now time.Time) (Anchor, error) {
	a, found, err := s.loadAnchor(ctx, date)
	if err != nil || found {
		return a, err
	}
	heads, err := s.Chains.Heads(ctx, sys)
	if err != nil {
		return Anchor{}, fmt.Errorf("service: anchor %s read heads: %w", date, err)
	}
	a, body, err := BuildAnchor(date, heads, now)
	if err != nil {
		return Anchor{}, err
	}
	if err := s.Store.PutAnchor(ctx, AnchorKey(date), body); err != nil {
		return Anchor{}, fmt.Errorf("service: anchor %s put: %w", date, err)
	}
	s.Logger.InfoContext(ctx, "audit anchor written", "date", date, "chains", len(a.Chains), "sha256", a.SHA256)
	return a, nil
}

// loadAnchor 读并解析锚点（最早版本）；不存在返回 found=false。整体 sha256 不符视为锚点被篡改：告警并返回 ErrAnchorDigest。
// 版本数 >1 说明锚点被重写或删除过（正常流程只写一次）：P0 告警，仍按最早版本继续。
func (s *AnchorService) loadAnchor(ctx context.Context, date string) (Anchor, bool, error) {
	key := AnchorKey(date)
	obj, err := s.Store.GetAnchor(ctx, key)
	if errors.Is(err, repository.ErrAnchorNotFound) {
		return Anchor{}, false, nil
	}
	if err != nil {
		return Anchor{}, false, fmt.Errorf("service: anchor %s get: %w", date, err)
	}
	s.checkVersions(ctx, key, obj)
	a, err := ParseAnchor(obj.Body)
	if err == nil && a.Date != date { // 他日锚点被复制到本 key：sha256 只覆盖 chains，date 单独核对
		err = fmt.Errorf("%w: date field %q != key date %s", ErrAnchorDigest, a.Date, date)
	}
	if errors.Is(err, ErrAnchorDigest) {
		s.Logger.ErrorContext(ctx, "audit anchor digest mismatch", "date", date, "err", err)
		s.Alerts.Fire(ctx, model.Alert{Name: model.AlertAuditChainBroken, Level: model.AlertP0,
			Detail: fmt.Sprintf("anchor %s sha256 mismatch", date)})
	}
	if err != nil {
		return Anchor{}, false, fmt.Errorf("service: anchor %s: %w", date, err)
	}
	return a, true, nil
}

// ensureVerified 校验前一日锚点到当日锚点的增量，写 verify/{date}.json 标记（链断也写，告警只发一次）。
// 前一日锚点缺失或被篡改时从链首校验。标记已存在（他进程 / 重启）时不重写，但静默重算一遍与标记比对：
// 不一致（标记被改写，或标记之后链被篡改）发一条 P0。
func (s *AnchorService) ensureVerified(ctx context.Context, today Anchor, prevDate string, now time.Time) error {
	key := AnchorVerifyKey(today.Date)
	stored, err := s.Store.GetAnchor(ctx, key)
	exists := err == nil
	if err != nil && !errors.Is(err, repository.ErrAnchorNotFound) {
		return fmt.Errorf("service: anchor verify mark %s: %w", today.Date, err)
	}
	var problems []string
	prev, found, err := s.loadAnchor(ctx, prevDate)
	switch {
	case errors.Is(err, ErrAnchorDigest):
		problems = append(problems, fmt.Sprintf("anchor %s sha256 mismatch", prevDate))
		prev = Anchor{}
	case err != nil:
		return err
	case !found:
		s.Logger.WarnContext(ctx, "audit anchor: previous anchor missing, verifying from chain start", "prev_date", prevDate)
	}
	mark, err := s.verifyIncrement(ctx, prev, today, !exists)
	if err != nil {
		return err
	}
	mark.Problems = append(problems, mark.Problems...)
	mark.PrevDate = prevDate
	if exists {
		s.checkVersions(ctx, key, stored)
		s.recheckMark(ctx, stored.Body, mark)
		return nil
	}
	mark.VerifiedAt = now.UTC().Format("2006-01-02T15:04:05.000Z")
	body, err := json.MarshalIndent(mark, "", "  ")
	if err != nil {
		return fmt.Errorf("service: marshal verify mark: %w", err)
	}
	if err := s.Store.PutAnchor(ctx, key, body); err != nil {
		return fmt.Errorf("service: anchor verify mark %s put: %w", today.Date, err)
	}
	s.Logger.InfoContext(ctx, "audit anchor verified", "date", today.Date, "rows", mark.Rows, "problems", len(mark.Problems))
	return nil
}

// recheckMark 比对已有标记与本次重算（verified_at 除外）；解析失败或任一字段不同即 P0。
func (s *AnchorService) recheckMark(ctx context.Context, body []byte, want anchorVerifyMark) {
	var got anchorVerifyMark
	err := json.Unmarshal(body, &got)
	if err == nil && got.Date == want.Date && got.PrevDate == want.PrevDate && got.Chains == want.Chains &&
		got.Rows == want.Rows && slices.Equal(got.Problems, want.Problems) {
		return
	}
	detail := fmt.Sprintf("verify mark %s differs from recomputation: stored rows=%d problems=%d, now rows=%d problems=%d",
		want.Date, got.Rows, len(got.Problems), want.Rows, len(want.Problems))
	if err != nil {
		detail = fmt.Sprintf("verify mark %s unreadable: %v", want.Date, err)
	}
	s.fireBroken(ctx, repository.ChainKey{}, detail)
}

// checkVersions 在锚点 bucket 对象版本数 >1（被重写 / 删除过）时发 P0。
func (s *AnchorService) checkVersions(ctx context.Context, key string, obj repository.AnchorObject) {
	if obj.Versions > 1 {
		s.fireBroken(ctx, repository.ChainKey{}, fmt.Sprintf("anchor object %s has %d versions (rewritten)", key, obj.Versions))
	}
}

// verifyIncrement 核对两份锚点仍与库一致，再逐链 verify (prev.chain_seq, today.chain_seq]；fire 时问题逐链告警
// （复核已有标记时不逐条告警，只在与标记不一致时由 recheckMark 汇总告警一次）。
func (s *AnchorService) verifyIncrement(ctx context.Context, prev, today Anchor, fire bool) (anchorVerifyMark, error) {
	mark := anchorVerifyMark{Date: today.Date, Chains: len(today.Chains), Problems: []string{}}
	report := func(k repository.ChainKey, p, detail string) {
		if fire {
			s.fireBroken(ctx, k, detail)
		}
		mark.Problems = append(mark.Problems, p)
	}
	for _, a := range []Anchor{prev, today} {
		mm, err := CheckAnchor(ctx, s.Chains, a)
		if err != nil {
			return mark, fmt.Errorf("service: check anchor %s: %w", a.Date, err)
		}
		for _, m := range mm {
			p := fmt.Sprintf("anchor %s: %s", a.Date, m)
			report(m.Key, p, p)
		}
	}
	prevSeq := make(map[repository.ChainKey]uint64, len(prev.Chains))
	for _, c := range prev.Chains {
		prevSeq[repository.ChainKey{TenantID: c.TenantID, FarmID: c.FarmID}] = c.ChainSeq
	}
	for _, c := range today.Chains {
		k := repository.ChainKey{TenantID: c.TenantID, FarmID: c.FarmID}
		from := prevSeq[k] + 1
		if c.ChainSeq < from {
			continue // 当日无增量（截尾已由 CheckAnchor 报出）
		}
		res, err := VerifyChain(ctx, s.Chains, k, from, c.ChainSeq, nil)
		if errors.Is(err, repository.ErrAuditChainHeadMissing) {
			continue // CheckAnchor 已报 HEAD_MISSING
		}
		if err != nil {
			return mark, fmt.Errorf("service: verify chain (%d,%d): %w", k.TenantID, k.FarmID, err)
		}
		mark.Rows += res.Rows
		if res.Break != nil {
			p := fmt.Sprintf("chain (%d,%d) %s", k.TenantID, k.FarmID, res.Break)
			report(k, p, "verify: "+p)
		}
	}
	return mark, nil
}

func (s *AnchorService) fireBroken(ctx context.Context, k repository.ChainKey, detail string) {
	s.Logger.ErrorContext(ctx, "audit chain broken", "tenant_id", k.TenantID, "farm_id", k.FarmID, "detail", detail)
	s.Alerts.Fire(ctx, model.Alert{Name: model.AlertAuditChainBroken, Level: model.AlertP0,
		Scope: model.Scope{TenantID: k.TenantID, FarmID: k.FarmID}, Detail: detail})
}
