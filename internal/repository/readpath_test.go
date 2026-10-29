package repository_test

import (
	"regexp"
	"strings"
	"testing"

	"agri-steward/internal/repository"
)

// 验收 6：指标与 run 详情的 SQL 不带库名前缀、不点名评测库，只能读到调用方注入的默认库（生产 agri_steward）；
// 试验库 agri_eval_t_{n} 与结果库 agri_eval 都不会出现在看板数据里。
func TestReadPathSQL_NoCrossSchema(t *testing.T) {
	// 表引用形如 `FROM db.table` / `JOIN db.table`：库名前缀即跨库读。
	qualified := regexp.MustCompile("(?i)\\b(from|join)\\s+`?[a-z_][a-z0-9_]*`?\\.`?[a-z_]")
	all := append(append([]string{}, repository.MetricSQL...), repository.RunDetailSQL...)
	if len(repository.MetricSQL) == 0 || len(repository.RunDetailSQL) == 0 {
		t.Fatal("SQL lists empty")
	}
	for _, q := range all {
		low := strings.ToLower(q)
		if strings.Contains(low, "agri_eval") || strings.Contains(low, "agri_steward") {
			t.Errorf("SQL names a schema: %s", q)
		}
		if m := qualified.FindString(q); m != "" {
			t.Errorf("schema-qualified table %q in: %s", m, q)
		}
		if !strings.Contains(low, "tenant_id=?") || !strings.Contains(low, "farm_id=?") {
			t.Errorf("SQL not scoped by tenant/farm: %s", q)
		}
	}
}
