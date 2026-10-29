package repository

import (
	"context"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
)

// 按月 RANGE COLUMNS(created_at) 分区的表（0001 已建 p202610…p202709 + pmax）。表名不能参数化，只认白名单。
const (
	TableAuditLog = "audit_log"
	TableRunEvent = "run_event"
	partitionMax  = "pmax"
)

var partitionedTables = map[string]bool{TableAuditLog: true, TableRunEvent: true}

// PartitionRepo 做月分区维护：列分区、拆 pmax、统计 pmax 行数。DDL 用应用账号执行（与迁移账号相同时）。
type PartitionRepo struct{ DB *gorm.DB }

func checkPartitioned(table string) error {
	if !partitionedTables[table] {
		return fmt.Errorf("repository: table %q is not a maintained partitioned table", table)
	}
	return nil
}

// PartitionName 是月分区名 pYYYYMM。
func PartitionName(month time.Time) string { return month.Format("p200601") }

// Partitions 列出表的分区名（information_schema，当前库）。
func (r PartitionRepo) Partitions(ctx context.Context, table string) ([]string, error) {
	if err := checkPartitioned(table); err != nil {
		return nil, err
	}
	var names []string
	err := r.DB.WithContext(ctx).Raw(`SELECT PARTITION_NAME FROM information_schema.PARTITIONS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND PARTITION_NAME IS NOT NULL ORDER BY PARTITION_ORDINAL_POSITION`,
		table).Scan(&names).Error
	if err != nil {
		return nil, fmt.Errorf("repository: list partitions of %s: %w", table, err)
	}
	return names, nil
}

// SplitMax 把 pmax 拆成 months 各月分区 + 新 pmax（months 须升序且都晚于现有最大月分区）。
// 每个月分区上界为下月 1 日，与 0001 口径一致。
func (r PartitionRepo) SplitMax(ctx context.Context, table string, months []time.Time) error {
	if err := checkPartitioned(table); err != nil {
		return err
	}
	if len(months) == 0 {
		return nil
	}
	parts := make([]string, 0, len(months)+1)
	for _, m := range months {
		next := time.Date(m.Year(), m.Month()+1, 1, 0, 0, 0, 0, time.UTC)
		parts = append(parts, fmt.Sprintf("PARTITION %s VALUES LESS THAN ('%s')", PartitionName(m), next.Format(time.DateOnly)))
	}
	parts = append(parts, "PARTITION "+partitionMax+" VALUES LESS THAN (MAXVALUE)")
	ddl := fmt.Sprintf("ALTER TABLE %s REORGANIZE PARTITION %s INTO (%s)", table, partitionMax, strings.Join(parts, ", "))
	if err := r.DB.WithContext(ctx).Exec(ddl).Error; err != nil {
		return fmt.Errorf("repository: reorganize %s pmax: %w", table, err)
	}
	return nil
}

// CountMax 统计落入 pmax 的行数（非 0 说明月分区没预建到位）。
func (r PartitionRepo) CountMax(ctx context.Context, table string) (int64, error) {
	if err := checkPartitioned(table); err != nil {
		return 0, err
	}
	var n int64
	if err := r.DB.WithContext(ctx).Raw("SELECT COUNT(*) FROM " + table + " PARTITION (" + partitionMax + ")").
		Scan(&n).Error; err != nil {
		return 0, fmt.Errorf("repository: count %s pmax: %w", table, err)
	}
	return n, nil
}
