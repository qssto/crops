package service

import (
	"context"
	"errors"
	"fmt"

	"agri-steward/internal/repository"
)

// 审计链校验（P1 §6.8 技术实现第 4 条）：按 chain_seq 流式重算，报第一个断点。auditctl verify、每日锚点、审计导出共用。

// BreakKind 是链断点类型。
type BreakKind string

const (
	BreakGap         BreakKind = "GAP"          // chain_seq 不连续（删行）
	BreakLinkBroken  BreakKind = "LINK_BROKEN"  // prev_hash 不等于前一行 row_hash，或末行与 head 不符
	BreakRowModified BreakKind = "ROW_MODIFIED" // 行内容重算的 row_hash 与存储值不符
)

// verifyPageSize 是流式校验每页读取的行数。
const verifyPageSize = 1000

// ChainBreak 是校验发现的第一个断点；AuditID 为 0 表示断点不落在某一行上（如尾部缺行、head 不符）。
type ChainBreak struct {
	Kind     BreakKind
	ChainSeq uint64
	AuditID  uint64
	Detail   string
}

func (b ChainBreak) String() string {
	return fmt.Sprintf("%s chain_seq=%d audit_log.id=%d %s", b.Kind, b.ChainSeq, b.AuditID, b.Detail)
}

// ChainReader 是校验需要的读能力，repository.AuditChainRepo 实现。
type ChainReader interface {
	Head(ctx context.Context, k repository.ChainKey) (repository.ChainHead, error)
	Rows(ctx context.Context, k repository.ChainKey, from, to uint64) ([]repository.ChainRow, error)
}

// VerifyResult 是一次校验的结论；Break 为 nil 表示 [From, To] 区间链完整。
type VerifyResult struct {
	Key      repository.ChainKey
	From, To uint64
	Rows     uint64
	Break    *ChainBreak
}

// VerifyChain 校验链 k 的 [from, to]（from=0 取 1，to=0 取 head）；to 等于 head 时还核对末行 row_hash 与 head_hash。
// from > 1 时以 from-1 行的 row_hash 作为期望 prev_hash（该行缺失即 GAP）。visit 非空时按序回调每一行（导出边校验边收集）。
func VerifyChain(ctx context.Context, r ChainReader, k repository.ChainKey, from, to uint64,
	visit func(repository.ChainRow) error) (VerifyResult, error) {
	head, err := r.Head(ctx, k)
	if err != nil {
		return VerifyResult{}, err
	}
	if from == 0 {
		from = 1
	}
	if to == 0 || to > head.ChainSeq {
		to = head.ChainSeq
	}
	res := VerifyResult{Key: k, From: from, To: to}
	if from > to {
		return res, nil // 空链或空区间
	}
	prev := repository.GenesisHash
	if from > 1 {
		before, err := r.Rows(ctx, k, from-1, from-1)
		if err != nil {
			return res, err
		}
		if len(before) == 0 {
			res.Break = &ChainBreak{Kind: BreakGap, ChainSeq: from - 1, Detail: "row before range missing"}
			return res, nil
		}
		prev = before[0].RowHash
	}
	next := from
	for next <= to {
		end := min(next+verifyPageSize-1, to)
		rows, err := r.Rows(ctx, k, next, end)
		if err != nil {
			return res, err
		}
		for _, row := range rows {
			if b, err := checkRow(row, next, prev); err != nil || b != nil {
				res.Break = b
				return res, err
			}
			if visit != nil {
				if err := visit(row); err != nil {
					return res, err
				}
			}
			prev, next = row.RowHash, next+1
			res.Rows++
		}
		if next <= end { // 本页尾部缺行
			res.Break = &ChainBreak{Kind: BreakGap, ChainSeq: next, Detail: "row missing"}
			return res, nil
		}
	}
	if to == head.ChainSeq && prev != head.HeadHash {
		res.Break = &ChainBreak{Kind: BreakLinkBroken, ChainSeq: to, Detail: "last row_hash != audit_chain_head.head_hash"}
	}
	return res, nil
}

// checkRow 按 GAP → LINK_BROKEN → ROW_MODIFIED 的顺序检查一行。
func checkRow(row repository.ChainRow, wantSeq uint64, wantPrev string) (*ChainBreak, error) {
	if row.ChainSeq != wantSeq {
		return &ChainBreak{Kind: BreakGap, ChainSeq: wantSeq, AuditID: row.ID,
			Detail: fmt.Sprintf("next row has chain_seq=%d", row.ChainSeq)}, nil
	}
	if row.PrevHash != wantPrev {
		return &ChainBreak{Kind: BreakLinkBroken, ChainSeq: row.ChainSeq, AuditID: row.ID, Detail: "prev_hash mismatch"}, nil
	}
	got, err := repository.AuditRowHash(row.PrevHash, row)
	if errors.Is(err, repository.ErrNotCanonicalizable) {
		return &ChainBreak{Kind: BreakRowModified, ChainSeq: row.ChainSeq, AuditID: row.ID, Detail: "summary not canonicalizable"}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("service: rehash audit_log %d: %w", row.ID, err)
	}
	if got != row.RowHash {
		return &ChainBreak{Kind: BreakRowModified, ChainSeq: row.ChainSeq, AuditID: row.ID, Detail: "row_hash mismatch"}, nil
	}
	return nil, nil
}
