package store

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// SampleRef 是样本身份（id@version），毕业判定按版本区分：改过题的样本须在新版本上重新连续通过。
type SampleRef struct {
	ID      string
	Version int
}

// graduateBatch 是单条 IN 列表的样本上限（每个 2 个占位符，远低于 MySQL 65535 占位符上限）。
const graduateBatch = 500

// GraduateTrials 是每次 nightly 里每个样本至少要有的 trial 数（路线 §6.2「连续 3 次夜间全量 pass^5 = 1」的 k；nightly 须 --n 不小于它）。
const GraduateTrials = 5

// graduateSQL 是路线 §6.2 的毕业判定：最近 3 次 nightly（DONE）都在 capability 套件下跑过、每次至少 GraduateTrials 个
// trial 且零失败。内层按 (样本, 版本, 实验) 判定单次 pass^k，外层要求 3 次都满足：只取路线原文的「3 次 + 零失败」时，
// --n 1 或 --tags 子集的 nightly 也会凑够毕业。「%s」处展开为 (?, ?), ... 占位符。
const graduateSQL = `SELECT p.sample_id
FROM (SELECT t.sample_id, t.sample_version, t.experiment_id
      FROM trial t
      JOIN (SELECT id FROM experiment
            WHERE trigger_kind = 'nightly' AND status = 'DONE'
            ORDER BY id DESC LIMIT 3) e ON e.id = t.experiment_id
      WHERE t.suite = 'capability' AND (t.sample_id, t.sample_version) IN (%s)
      GROUP BY t.sample_id, t.sample_version, t.experiment_id
      HAVING COUNT(DISTINCT t.trial_index) >= ? AND SUM(t.passed = 0) = 0) p
GROUP BY p.sample_id, p.sample_version
HAVING COUNT(*) = 3`

// Graduates 返回 refs 中可从 capability 毕业到 regression 的样本 id（按 id 排序，分批查询）。
func (s *Experiments) Graduates(ctx context.Context, refs []SampleRef) ([]string, error) {
	var out []string
	for start := 0; start < len(refs); start += graduateBatch {
		batch := refs[start:min(start+graduateBatch, len(refs))]
		query, args := graduateQuery(batch)
		var ids []string
		if err := s.db.WithContext(ctx).Raw(query, args...).Scan(&ids).Error; err != nil {
			return nil, fmt.Errorf("store: graduates: %w", err)
		}
		out = append(out, ids...)
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// graduateQuery 为非空 refs 展开占位符与参数。
func graduateQuery(refs []SampleRef) (string, []any) {
	ph := make([]string, len(refs))
	args := make([]any, 0, 2*len(refs)+1)
	for i, r := range refs {
		ph[i] = "(?, ?)"
		args = append(args, r.ID, r.Version)
	}
	return fmt.Sprintf(graduateSQL, strings.Join(ph, ", ")), append(args, GraduateTrials)
}
