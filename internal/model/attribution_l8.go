package model

import (
	"cmp"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// P2 L8 归因周报（§6.9 技术实现 ① 2-3、② attribution_issue）：按「原因 × 问题类型 × 主 kb_id」聚合上周的负向信号。

// 归因原因（attribution_issue.reason，varchar(16)）：驳回 / 差评沿用驳回五类（RejectReasonCode），另三类为系统信号。
// VALIDATION_FAILED 有 17 个字符放不进 varchar(16)，记作 GUARDRAIL（护栏拦截）。
const (
	AttrReasonGuardrail   = "GUARDRAIL"    // advice_card refuse_reason=VALIDATION_FAILED
	AttrReasonKBMiss      = "KB_MISS"      // advice_card refuse_reason=KB_MISS
	AttrReasonExpertWrong = "EXPERT_WRONG" // online_judge_result 专家回写最低分
)

// 证据来源（evidence_refs[].source）。
const (
	EvidenceRejection  = "rejection"  // rejection_reason.id
	EvidenceFeedback   = "feedback"   // advice_feedback.id（DISLIKE）
	EvidenceValidation = "validation" // advice_card.id（VALIDATION_FAILED 拒收）
	EvidenceKBMiss     = "kb_miss"    // advice_card.id（KB_MISS 拒答）
	EvidenceExpert     = "expert"     // online_judge_result.id
)

// attribution_issue.status。
const (
	IssueOpen       = "OPEN"
	IssueInProgress = "IN_PROGRESS"
	IssueResolved   = "RESOLVED"
	IssueVerified   = "VERIFIED"
)

// attribution_issue.disposition（§6.9 ②）。
const (
	DispositionAddKB       = "ADD_KB"
	DispositionFixLoader   = "FIX_LOADER"
	DispositionOffloadRule = "OFFLOAD_RULE"
	DispositionFixPrompt   = "FIX_PROMPT"
	DispositionWontFix     = "WONTFIX"
)

// MaxEvidenceRefs 是 evidence_refs 保留的最近条数。
const MaxEvidenceRefs = 50

// MaxResolutionRef 是 resolution_ref 的列宽（varchar(256)，按字符计）。
const MaxResolutionRef = 256

// ErrAttributionPatch 是处置更新入参非法。
var ErrAttributionPatch = errors.New("ATTRIBUTION_PATCH_INVALID")

// AttributionHit 是一条负向信号（一行驳回 / 差评 / 拒收 / 拒答 / 专家判错）。PrimaryKB 为空表示该建议卡无 kb 引用。
type AttributionHit struct {
	TenantID, FarmID uint64
	Reason           string
	QuestionType     string
	PrimaryKB        string
	RunID            uint64
	Source           string
	SourceID         uint64
	At               time.Time
}

// Signature 是 md5(原因|问题类型|主 kb_id) 的十六进制（attribution_issue.signature char(32)）。
func Signature(reason, questionType, primaryKB string) string {
	sum := md5.Sum([]byte(reason + "|" + questionType + "|" + primaryKB))
	return hex.EncodeToString(sum[:])
}

// Signature 返回该信号所属问题的签名。
func (h AttributionHit) Signature() string { return Signature(h.Reason, h.QuestionType, h.PrimaryKB) }

// EvidenceRef 是 evidence_refs 的一项。
type EvidenceRef struct {
	RunID    uint64 `json:"run_id"`
	Source   string `json:"source"`
	SourceID uint64 `json:"source_id"`
	Week     string `json:"week"`
}

// MergeEvidence 把本周证据放在前面、接上已有证据，按 (source, source_id) 去重（先出现者留），截到 MaxEvidenceRefs 条。
func MergeEvidence(fresh, old []EvidenceRef) []EvidenceRef {
	type key struct {
		source string
		id     uint64
	}
	seen := make(map[key]bool, len(fresh)+len(old))
	out := make([]EvidenceRef, 0, min(len(fresh)+len(old), MaxEvidenceRefs))
	for _, r := range slices.Concat(fresh, old) {
		k := key{r.Source, r.SourceID}
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, r)
		if len(out) == MaxEvidenceRefs {
			break
		}
	}
	return out
}

// AttributionIssue 是 attribution_issue 的一行（平台行 tenant_id=0, farm_id=0）。
type AttributionIssue struct {
	ID            uint64        `json:"id"`
	Signature     string        `json:"signature"`
	FirstWeek     string        `json:"first_week"`
	LastWeek      string        `json:"last_week"`
	Reason        string        `json:"reason"`
	QuestionType  string        `json:"question_type"`
	PrimaryKB     *string       `json:"primary_kb_id"`
	HitCount      int           `json:"hit_count"`
	Evidence      []EvidenceRef `json:"evidence_refs"`
	Disposition   *string       `json:"disposition"`
	OwnerID       *uint64       `json:"owner_id"`
	Status        string        `json:"status"`
	ReopenCount   int           `json:"reopen_count"`
	ResolutionRef *string       `json:"resolution_ref"`
	RowVer        int           `json:"row_ver"`
	CreatedAt     time.Time     `json:"created_at"`
	UpdatedAt     time.Time     `json:"updated_at"`
}

// Closed 报告该问题是否已处置关闭（再次命中即重开）。
func (i AttributionIssue) Closed() bool {
	return i.Status == IssueResolved || i.Status == IssueVerified
}

// IssueUpsert 是周报对一个签名的写入（evidence 已与旧值合并）。
type IssueUpsert struct {
	Signature    string
	Week         string
	Reason       string
	QuestionType string
	PrimaryKB    string
	HitCount     int
	Evidence     []EvidenceRef
}

// AggregateHits 按签名聚合本周信号：命中数、证据（按时间新到旧，同时刻按来源、来源 id）。
// 结果按命中数降序、签名升序。
func AggregateHits(week string, hits []AttributionHit) []IssueUpsert {
	bySig := map[string]*IssueUpsert{}
	byTime := slices.Clone(hits)
	slices.SortStableFunc(byTime, func(a, b AttributionHit) int {
		return cmp.Or(b.At.Compare(a.At), cmp.Compare(a.Source, b.Source), cmp.Compare(a.SourceID, b.SourceID))
	})
	for _, h := range byTime {
		sig := h.Signature()
		u, ok := bySig[sig]
		if !ok {
			u = &IssueUpsert{Signature: sig, Week: week, Reason: h.Reason, QuestionType: h.QuestionType, PrimaryKB: h.PrimaryKB}
			bySig[sig] = u
		}
		u.HitCount++
		u.Evidence = append(u.Evidence, EvidenceRef{RunID: h.RunID, Source: h.Source, SourceID: h.SourceID, Week: week})
	}
	out := make([]IssueUpsert, 0, len(bySig))
	for _, u := range bySig {
		out = append(out, *u)
	}
	slices.SortFunc(out, func(a, b IssueUpsert) int {
		return cmp.Or(cmp.Compare(b.HitCount, a.HitCount), cmp.Compare(a.Signature, b.Signature))
	})
	return out
}

// ISOWeek 返回 t 所在 ISO 周（如 2026W45，attribution_issue.first_week / last_week char(8)）。
func ISOWeek(t time.Time) string {
	y, w := t.ISOWeek()
	return fmt.Sprintf("%04dW%02d", y, w)
}

// WeekStart 返回 t 在其时区下所在 ISO 周的周一 00:00。
func WeekStart(t time.Time) time.Time {
	d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
	offset := (int(d.Weekday()) + 6) % 7 // 周一 = 0
	return d.AddDate(0, 0, -offset)
}

// AttributionPatch 是处置更新（PATCH .../attribution-issues/{id}）。字段为 nil 表示不改；RowVer 必填（乐观锁）。
type AttributionPatch struct {
	Disposition   *string `json:"disposition"`
	OwnerID       *uint64 `json:"owner_id"`
	ResolutionRef *string `json:"resolution_ref"`
	Status        *string `json:"status"`
	RowVer        *int    `json:"row_ver"`
}

var (
	dispositions  = []string{DispositionAddKB, DispositionFixLoader, DispositionOffloadRule, DispositionFixPrompt, DispositionWontFix}
	issueStatuses = []string{IssueOpen, IssueInProgress, IssueResolved, IssueVerified}
)

// Validate 要求 row_ver 非负、至少改一项、处置 / 状态属枚举、owner_id 非 0、resolution_ref 非空且 ≤256 字符。
func (p AttributionPatch) Validate() error {
	switch {
	case p.RowVer == nil || *p.RowVer < 0:
		return fmt.Errorf("%w: row_ver required", ErrAttributionPatch)
	case p.Disposition == nil && p.OwnerID == nil && p.ResolutionRef == nil && p.Status == nil:
		return fmt.Errorf("%w: nothing to update", ErrAttributionPatch)
	case p.Disposition != nil && !slices.Contains(dispositions, *p.Disposition):
		return fmt.Errorf("%w: disposition %q", ErrAttributionPatch, *p.Disposition)
	case p.Status != nil && !slices.Contains(issueStatuses, *p.Status):
		return fmt.Errorf("%w: status %q", ErrAttributionPatch, *p.Status)
	case p.OwnerID != nil && *p.OwnerID == 0:
		return fmt.Errorf("%w: owner_id must be > 0", ErrAttributionPatch)
	case p.ResolutionRef != nil && (strings.TrimSpace(*p.ResolutionRef) == "" || len([]rune(*p.ResolutionRef)) > MaxResolutionRef):
		return fmt.Errorf("%w: resolution_ref must be 1..%d chars", ErrAttributionPatch, MaxResolutionRef)
	}
	return nil
}
