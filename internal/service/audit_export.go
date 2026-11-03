package service

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strconv"
	"time"

	"agri-steward/internal/clock"
	"agri-steward/internal/model"
	"agri-steward/internal/piiclean"
	"agri-steward/internal/repository"
)

// P1 L7 审计导出（§6.8 技术实现第 5 条）：POST 只写 outbox(topic=audit.export)；worker 处理器读链区间 → verify →
// 逐行脱敏 → zip → RawStore.Put(kind=EXPORT, run 0) → 回写 raw_object_id → pii_clean 扫出口文件；GET 查进度并签发 5 分钟 URL。

// 导出文件内的条目名。
const (
	exportLogEntry      = "audit_log.jsonl"
	exportManifestEntry = "manifest.json"
)

// exportRequestIDPattern 限定 request_id（进 dedup_key，varchar(128)）。
var exportRequestIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)

// AuditExportStore 是导出 outbox 行的读写，repository.AuditExportRepo 实现。
type AuditExportStore interface {
	InsertTx(ctx context.Context, tx *repository.DB, scope model.Scope, p model.AuditExportPayload, now time.Time) (uint64, bool, error)
	Get(ctx context.Context, scope model.Scope, id uint64) (repository.AuditExportRow, error)
	SetPayloadTx(ctx context.Context, tx *repository.DB, scope model.Scope, id uint64, p model.AuditExportPayload, now time.Time) error
}

// ExportChains 是导出读链能力，repository.AuditChainRepo 实现。
type ExportChains interface {
	ChainReader
	SeqRange(ctx context.Context, k repository.ChainKey, from, to time.Time) (lo, hi uint64, ok bool, err error)
	SeqBefore(ctx context.Context, k repository.ChainKey, t time.Time) (uint64, error)
}

// RawPutter 写对象并登记 raw_object（agent.RawStore 同签名），repository.RawStore 实现。
type RawPutter interface {
	Put(ctx context.Context, p model.RawPut) (model.RawObject, error)
}

// RawTombstoner 在事务内置 raw_object.deleted_at，repository.RetentionRepo 实现。
type RawTombstoner interface {
	MarkDeletedTx(ctx context.Context, tx *repository.DB, ref repository.RawRef, now time.Time) (bool, error)
}

// AuditExportDeps 是 AuditExportService 的依赖；MaskText / MaskJSON 由装配注入 guardrail/pii 的 Mask / MaskJSON。
type AuditExportDeps struct {
	Store     AuditExportStore
	Chains    ExportChains
	Raw       RawPutter
	RawAccess RawAccessStore // 事务、读 raw_object 行、同事务审计
	Tombstone RawTombstoner
	Objects   repository.ObjectRemover
	RawBucket string
	Presign   repository.Presigner
	Perms     *PermissionMatrix
	Alerts    model.AlertSink
	Clock     clock.Clock
	Logger    *slog.Logger
	MaxSpan   time.Duration
	MaskText  func(string) string
	MaskJSON  func(json.RawMessage) json.RawMessage
}

// AuditExportService 处理审计导出请求、worker 生成与进度查询。
type AuditExportService struct{ d AuditExportDeps }

// NewAuditExportService 校验依赖（Logger 可空）。
func NewAuditExportService(d AuditExportDeps) (*AuditExportService, error) {
	if d.Store == nil || d.Chains == nil || d.Raw == nil || d.RawAccess == nil || d.Tombstone == nil || d.Objects == nil ||
		d.RawBucket == "" || d.Presign == nil || d.Perms == nil || d.Alerts == nil || d.Clock == nil || d.MaxSpan <= 0 ||
		d.MaskText == nil || d.MaskJSON == nil {
		return nil, errors.New("service: audit export service needs Store, Chains, Raw, RawAccess, Tombstone, Objects, " +
			"RawBucket, Presign, Perms, Alerts, Clock, MaxSpan, MaskText and MaskJSON")
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	return &AuditExportService{d: d}, nil
}

// AuditExportRequest 是 POST 请求体：区间 [From, To) 按 audit_log.created_at；RequestID 是幂等键。
type AuditExportRequest struct {
	From      time.Time `json:"from"`
	To        time.Time `json:"to"`
	RequestID string    `json:"request_id"`
}

// AuditExportStatus 是导出进度；DONE 时带 5 分钟预签名 URL（URL 是凭据，不进审计）。
type AuditExportStatus struct {
	ExportID  uint64     `json:"export_id"`
	State     string     `json:"state"`
	Error     string     `json:"error,omitempty"`
	From      time.Time  `json:"from"`
	To        time.Time  `json:"to"`
	Rows      uint64     `json:"rows"`
	ChainFrom uint64     `json:"chain_from,omitempty"`
	ChainTo   uint64     `json:"chain_to,omitempty"`
	URL       string     `json:"url,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// Request 校验权限与区间后写一条 audit.export（同农场同 request_id 幂等，返回已有导出）。
func (s *AuditExportService) Request(ctx context.Context, scope model.Scope, req AuditExportRequest) (AuditExportStatus, error) {
	if !s.d.Perms.Allows(OpAuditExport, scope.Roles...) {
		return AuditExportStatus{}, fmt.Errorf("%w: %s", ErrForbidden, OpAuditExport)
	}
	switch {
	case req.From.IsZero() || req.To.IsZero():
		return AuditExportStatus{}, fmt.Errorf("%w: from and to required", ErrBadInput)
	case !req.From.Before(req.To):
		return AuditExportStatus{}, fmt.Errorf("%w: from must be before to", ErrBadInput)
	case req.To.Sub(req.From) > s.d.MaxSpan:
		return AuditExportStatus{}, fmt.Errorf("%w: span %s over max %s", ErrBadInput, req.To.Sub(req.From), s.d.MaxSpan)
	case !exportRequestIDPattern.MatchString(req.RequestID):
		return AuditExportStatus{}, fmt.Errorf("%w: request_id must match %s", ErrBadInput, exportRequestIDPattern)
	}
	now := s.d.Clock.Now()
	p := model.AuditExportPayload{FarmID: scope.FarmID, From: req.From.UTC(), To: req.To.UTC(), RequestID: req.RequestID,
		RequestedBy: scope.UserID, State: model.AuditExportPending}
	var id uint64
	err := s.d.RawAccess.InTx(ctx, func(tx *repository.DB) error {
		var err error
		id, _, err = s.d.Store.InsertTx(ctx, tx, scope, p, now)
		return err
	})
	if err != nil {
		return AuditExportStatus{}, fmt.Errorf("service: request audit export: %w", err)
	}
	return s.Status(ctx, scope, id)
}

// Status 返回导出进度：查询以 tenant_id, farm_id 开头（他场导出 → model.ErrNotFound → 404）。DONE 时签发 5 分钟 URL，
// 签发与 RAW_READ/OK 同事务（封链追加 SENSITIVE.RAW_READ）；文件已过保留期返回 model.ErrRawExpired。
func (s *AuditExportService) Status(ctx context.Context, scope model.Scope, id uint64) (AuditExportStatus, error) {
	if !s.d.Perms.Allows(OpAuditExport, scope.Roles...) {
		return AuditExportStatus{}, fmt.Errorf("%w: %s", ErrForbidden, OpAuditExport)
	}
	row, err := s.d.Store.Get(ctx, scope, id)
	if err != nil {
		return AuditExportStatus{}, fmt.Errorf("service: audit export %d: %w", id, err)
	}
	p := row.Payload
	st := AuditExportStatus{ExportID: row.ID, State: p.State, Error: p.Error, From: p.From, To: p.To, Rows: p.Rows,
		ChainFrom: p.ChainFrom, ChainTo: p.ChainTo, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
	if row.Status == "DEAD" && st.State == model.AuditExportPending { // outbox 重试耗尽
		st.State, st.Error = model.AuditExportFailed, "RETRY_EXHAUSTED"
	}
	if st.State != model.AuditExportDone || p.RawObjectID == 0 {
		return st, nil
	}
	now := s.d.Clock.Now()
	err = s.d.RawAccess.InTx(ctx, func(tx *repository.DB) error {
		raw, err := s.d.RawAccess.RawTx(ctx, tx, scope, p.RawObjectID)
		if err != nil {
			return err
		}
		if raw.Kind != model.RawExport {
			return fmt.Errorf("service: audit export %d raw_object %d kind %s: %w", id, raw.ID, raw.Kind, model.ErrNotFound)
		}
		url, err := s.d.Presign.PresignGet(ctx, raw.ObjectKey, RawReadTTL)
		if err != nil {
			return err
		}
		exp := now.Add(RawReadTTL)
		body, err := json.Marshal(rawAudit{RawID: raw.ID, Kind: string(raw.Kind), ExpiresAt: &exp})
		if err != nil {
			return fmt.Errorf("service: encode RAW_READ audit: %w", err)
		}
		rec := repository.NewAuditRecord(scope, nil, model.Actor{Type: model.ActorUser, ID: scope.UserID}, model.AuditRawRead,
			"raw:"+strconv.FormatUint(raw.ID, 10), body, model.AuditResultOK, now)
		rawID := raw.ID
		rec.RawObjectID = &rawID
		if err := s.d.RawAccess.AuditTx(ctx, tx, rec); err != nil {
			return err
		}
		st.URL, st.ExpiresAt = url, &exp
		return nil
	})
	if err != nil {
		return AuditExportStatus{}, fmt.Errorf("service: audit export %d download: %w", id, err)
	}
	return st, nil
}

// exportLine 是 audit_log.jsonl 的一行：target / summary 已脱敏，row_hash / prev_hash 按库内原值（脱敏前）给出。
type exportLine struct {
	ID            uint64          `json:"id"`
	ChainSeq      uint64          `json:"chain_seq"`
	PrevHash      string          `json:"prev_hash"`
	RowHash       string          `json:"row_hash"`
	RunID         *uint64         `json:"run_id"`
	ActorType     string          `json:"actor_type"`
	ActorID       uint64          `json:"actor_id"`
	Action        string          `json:"action"`
	Target        string          `json:"target"`
	PayloadDigest string          `json:"payload_digest"`
	Summary       json.RawMessage `json:"summary"`
	Result        string          `json:"result"`
	RawObjectID   *uint64         `json:"raw_object_id"`
	CreatedAt     string          `json:"created_at"`
}

// exportManifest 是 manifest.json。
type exportManifest struct {
	ExportID      uint64   `json:"export_id"`
	TenantID      uint64   `json:"tenant_id"`
	FarmID        uint64   `json:"farm_id"`
	From          string   `json:"from"`
	To            string   `json:"to"`
	ChainFrom     uint64   `json:"chain_from"`
	ChainTo       uint64   `json:"chain_to"`
	Rows          uint64   `json:"rows"`
	ChainVerified bool     `json:"chain_verified"`
	MaskedFields  []string `json:"masked_fields"`
	GeneratedAt   string   `json:"generated_at"`
}

const exportTimeLayout = "2006-01-02T15:04:05.000Z"

// Handle 是 audit.export 的 outbox 处理器。链断 / pii 命中写 FAILED 并告警后返回 nil（不重试）；
// 读库 / 对象存储的瞬时错误返回 error 交给 outbox 退避重试。已是终态的消息直接完成（重放幂等）。
func (s *AuditExportService) Handle(ctx context.Context, msg repository.OutboxMessage) error {
	var p model.AuditExportPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		return fmt.Errorf("service: audit export %d payload: %w", msg.ID, err)
	}
	if p.State == model.AuditExportDone || p.State == model.AuditExportFailed {
		return nil
	}
	scope := model.Scope{TenantID: msg.TenantID, FarmID: msg.FarmID, UserID: p.RequestedBy}
	k := repository.ChainKey{TenantID: msg.TenantID, FarmID: msg.FarmID}
	now := s.d.Clock.Now()
	lo, hi, ok, err := s.d.Chains.SeqRange(ctx, k, p.From, p.To)
	if err != nil {
		return fmt.Errorf("service: audit export %d range: %w", msg.ID, err)
	}
	vFrom, vTo, err := s.verifySpan(ctx, k, p, lo, hi, ok)
	if err != nil {
		return fmt.Errorf("service: audit export %d range: %w", msg.ID, err)
	}
	var lines bytes.Buffer
	var rows uint64
	res, err := VerifyChain(ctx, s.d.Chains, k, vFrom, vTo, func(r repository.ChainRow) error {
		// chain_seq 与 created_at 不严格同序，且校验区间多含窗口前后各一行：只导出 created_at 落在 [From, To) 的行
		if r.Rec.CreatedAt.Before(p.From) || !r.Rec.CreatedAt.Before(p.To) {
			return nil
		}
		rows++
		return s.writeLine(&lines, r)
	})
	if err != nil {
		return fmt.Errorf("service: audit export %d verify: %w", msg.ID, err)
	}
	if res.Break != nil {
		s.d.Logger.ErrorContext(ctx, "audit export: chain broken", "export_id", msg.ID, "break", res.Break.String())
		s.d.Alerts.Fire(ctx, model.Alert{Name: model.AlertAuditChainBroken, Level: model.AlertP0, Scope: scope,
			Detail: fmt.Sprintf("audit export %d: %s", msg.ID, res.Break)})
		return s.fail(ctx, scope, msg.ID, p, model.AuditExportErrChainBroken, now)
	}
	p.Rows, p.ChainFrom, p.ChainTo = rows, lo, hi
	body, err := s.buildZip(msg, p, lines.Bytes(), now)
	if err != nil {
		return err
	}
	// 对象 key 按入队时刻定日期：跨日重放仍写同一 key（覆盖旧对象、复用 raw_object 行），不留未扫描的孤儿对象
	at := msg.CreatedAt
	if at.IsZero() {
		at = now
	}
	obj, err := s.d.Raw.Put(ctx, model.RawPut{TenantID: msg.TenantID, FarmID: msg.FarmID, Kind: model.RawExport,
		Name: fmt.Sprintf("audit-export-%d.zip", msg.ID), At: at, Body: body, ContentType: "application/zip"})
	if err != nil {
		return fmt.Errorf("service: audit export %d put: %w", msg.ID, err)
	}
	p.RawObjectID = obj.ID
	if err := s.setPayload(ctx, scope, msg.ID, p, now); err != nil {
		return err
	}
	key, leak, hit, err := scanExport(body)
	if err != nil {
		return fmt.Errorf("service: audit export %d scan: %w", msg.ID, err)
	}
	if hit {
		return s.quarantine(ctx, scope, msg.ID, p, obj, key, leak, now)
	}
	return s.finish(ctx, scope, msg.ID, p, now)
}

// verifySpan 返回导出的校验区间：窗口 [lo, hi] 向前后各扩一行，使删窗口首 / 末行、把边界行 created_at 挪出窗口
// 都表现为 GAP / ROW_MODIFIED；窗口为空时校验「窗口前最后一行 b 与其后一行 b+1」的衔接（整窗删除即 GAP）。
// 上界超过 head 时由 VerifyChain 截到 head 并核对 head_hash。
func (s *AuditExportService) verifySpan(ctx context.Context, k repository.ChainKey, p model.AuditExportPayload,
	lo, hi uint64, ok bool) (uint64, uint64, error) {
	if ok {
		return max(lo-1, 1), hi + 1, nil
	}
	b, err := s.d.Chains.SeqBefore(ctx, k, p.From)
	if err != nil {
		return 0, 0, err
	}
	return max(b, 1), b + 1, nil
}

// writeLine 脱敏一行（pii_mask：target 按文本、summary 按 JSON 结构）后追加到 jsonl。
func (s *AuditExportService) writeLine(w *bytes.Buffer, r repository.ChainRow) error {
	summary := json.RawMessage("null")
	if len(r.Rec.Summary) > 0 {
		summary = s.d.MaskJSON(r.Rec.Summary)
	}
	b, err := json.Marshal(exportLine{ID: r.ID, ChainSeq: r.ChainSeq, PrevHash: r.PrevHash, RowHash: r.RowHash,
		RunID: r.Rec.RunID, ActorType: string(r.Rec.Actor.Type), ActorID: r.Rec.Actor.ID, Action: r.Rec.Action,
		Target: s.d.MaskText(r.Rec.Target), PayloadDigest: r.Rec.PayloadDigest, Summary: summary, Result: r.Rec.Result,
		RawObjectID: r.Rec.RawObjectID, CreatedAt: r.Rec.CreatedAt.UTC().Format(exportTimeLayout)})
	if err != nil {
		return fmt.Errorf("service: encode export line audit_log %d: %w", r.ID, err)
	}
	w.Write(b)
	w.WriteByte('\n')
	return nil
}

// buildZip 打包 audit_log.jsonl 与 manifest.json。
func (s *AuditExportService) buildZip(msg repository.OutboxMessage, p model.AuditExportPayload, lines []byte,
	now time.Time) ([]byte, error) {
	m, err := json.MarshalIndent(exportManifest{ExportID: msg.ID, TenantID: msg.TenantID, FarmID: msg.FarmID,
		From: p.From.UTC().Format(exportTimeLayout), To: p.To.UTC().Format(exportTimeLayout), ChainFrom: p.ChainFrom,
		ChainTo: p.ChainTo, Rows: p.Rows, ChainVerified: true, MaskedFields: []string{"target", "summary"},
		GeneratedAt: now.UTC().Format(exportTimeLayout)}, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("service: encode export manifest: %w", err)
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range []struct {
		name string
		body []byte
	}{{exportLogEntry, lines}, {exportManifestEntry, m}} {
		f, err := zw.CreateHeader(&zip.FileHeader{Name: e.name, Method: zip.Deflate, Modified: now.UTC()})
		if err != nil {
			return nil, fmt.Errorf("service: export zip %s: %w", e.name, err)
		}
		if _, err := f.Write(e.body); err != nil {
			return nil, fmt.Errorf("service: export zip write %s: %w", e.name, err)
		}
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("service: export zip close: %w", err)
	}
	return buf.Bytes(), nil
}

// scanExport 解压出口文件逐行跑 pii_clean（与评测打分器同一检测器），返回第一处命中（Leak.Value 已掩码）。
func scanExport(body []byte) (string, piiclean.Leak, bool, error) {
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return "", piiclean.Leak{}, false, fmt.Errorf("service: open export zip: %w", err)
	}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			return "", piiclean.Leak{}, false, fmt.Errorf("service: open export entry %s: %w", f.Name, err)
		}
		key, leak, hit, err := scanLines(rc)
		if cerr := rc.Close(); err == nil && cerr != nil {
			err = fmt.Errorf("service: close export entry %s: %w", f.Name, cerr)
		}
		if err != nil || hit {
			return f.Name + ":" + key, leak, hit, err
		}
	}
	return "", piiclean.Leak{}, false, nil
}

func scanLines(r io.Reader) (string, piiclean.Leak, bool, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	n := 0
	for sc.Scan() {
		n++
		if key, leak, hit := piiclean.FirstLeak(sc.Text()); hit {
			return fmt.Sprintf("line %d %s", n, key), leak, true, nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", piiclean.Leak{}, false, fmt.Errorf("service: scan export: %w", err)
	}
	return "", piiclean.Leak{}, false, nil
}

// quarantine 处理 pii 命中：删对象、置墓碑、写 FAILED(PII_LEAK)，并告警 audit_export_pii_leak（P0）。
// 删对象失败返回 error 重试（payload 仍为 PENDING，重放会重新生成并再扫）。
func (s *AuditExportService) quarantine(ctx context.Context, scope model.Scope, id uint64, p model.AuditExportPayload,
	obj model.RawObject, key string, leak piiclean.Leak, now time.Time) error {
	if err := s.d.Objects.RemoveObject(ctx, s.d.RawBucket, obj.ObjectKey); err != nil {
		return fmt.Errorf("service: audit export %d remove leaked object: %w", id, err)
	}
	p.State, p.Error = model.AuditExportFailed, model.AuditExportErrPIILeak
	rec, err := exportAuditRecord(scope, id, p, now)
	if err != nil {
		return err
	}
	err = s.d.RawAccess.InTx(ctx, func(tx *repository.DB) error {
		if _, err := s.d.Tombstone.MarkDeletedTx(ctx, tx, repository.RawRef{TenantID: scope.TenantID, FarmID: scope.FarmID,
			ID: obj.ID}, now); err != nil {
			return err
		}
		if err := s.d.Store.SetPayloadTx(ctx, tx, scope, id, p, now); err != nil {
			return err
		}
		return s.d.RawAccess.AuditTx(ctx, tx, rec)
	})
	if err != nil {
		return fmt.Errorf("service: audit export %d quarantine: %w", id, err)
	}
	s.d.Logger.ErrorContext(ctx, "audit export: pii_clean hit, object deleted", "export_id", id, "raw_object_id", obj.ID,
		"where", key, "kind", leak.Kind)
	s.d.Alerts.Fire(ctx, model.Alert{Name: model.AlertAuditExportPIILeak, Level: model.AlertP0, Scope: scope,
		Detail: fmt.Sprintf("audit export %d: %s %s", id, leak.Kind, key)})
	return nil
}

// fail 写 FAILED 终态，同事务写 AUDIT_EXPORT result=FAILED（summary 只含原因码，不含 PII）。
func (s *AuditExportService) fail(ctx context.Context, scope model.Scope, id uint64, p model.AuditExportPayload,
	code string, now time.Time) error {
	p.State, p.Error = model.AuditExportFailed, code
	return s.settle(ctx, scope, id, p, now)
}

func (s *AuditExportService) setPayload(ctx context.Context, scope model.Scope, id uint64, p model.AuditExportPayload,
	now time.Time) error {
	err := s.d.RawAccess.InTx(ctx, func(tx *repository.DB) error {
		return s.d.Store.SetPayloadTx(ctx, tx, scope, id, p, now)
	})
	if err != nil {
		return fmt.Errorf("service: audit export %d set payload: %w", id, err)
	}
	return nil
}

// finish 写 DONE，同事务写 AUDIT_EXPORT 审计（封链追加 SENSITIVE.AUDIT_EXPORT）。
func (s *AuditExportService) finish(ctx context.Context, scope model.Scope, id uint64, p model.AuditExportPayload,
	now time.Time) error {
	p.State, p.Error = model.AuditExportDone, ""
	return s.settle(ctx, scope, id, p, now)
}

// settle 把终态 payload 与 AUDIT_EXPORT 审计写在同一事务。
func (s *AuditExportService) settle(ctx context.Context, scope model.Scope, id uint64, p model.AuditExportPayload,
	now time.Time) error {
	rec, err := exportAuditRecord(scope, id, p, now)
	if err != nil {
		return err
	}
	err = s.d.RawAccess.InTx(ctx, func(tx *repository.DB) error {
		if err := s.d.Store.SetPayloadTx(ctx, tx, scope, id, p, now); err != nil {
			return err
		}
		return s.d.RawAccess.AuditTx(ctx, tx, rec)
	})
	if err != nil {
		return fmt.Errorf("service: audit export %d %s: %w", id, p.State, err)
	}
	return nil
}

// exportAuditRecord 按终态构造 AUDIT_EXPORT 审计：DONE → OK，FAILED → FAILED 且 summary 带原因码。
func exportAuditRecord(scope model.Scope, id uint64, p model.AuditExportPayload, now time.Time) (model.AuditRecord, error) {
	sum := map[string]any{"export_id": id, "from": p.From.UTC().Format(exportTimeLayout),
		"to": p.To.UTC().Format(exportTimeLayout), "rows": p.Rows, "chain_from": p.ChainFrom, "chain_to": p.ChainTo}
	result := model.AuditResultOK
	if p.State == model.AuditExportFailed {
		result, sum["error"] = model.AuditResultFailed, p.Error
	}
	summary, err := json.Marshal(sum)
	if err != nil {
		return model.AuditRecord{}, fmt.Errorf("service: encode export audit: %w", err)
	}
	rec := repository.NewAuditRecord(scope, nil, model.Actor{Type: model.ActorUser, ID: p.RequestedBy}, model.AuditExport,
		"export:"+strconv.FormatUint(id, 10), summary, result, now)
	if p.RawObjectID != 0 {
		rawID := p.RawObjectID
		rec.RawObjectID = &rawID
	}
	return rec, nil
}
