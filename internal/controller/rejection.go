package controller

import (
	"context"
	"encoding/csv"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"agri-steward/internal/model"
	"agri-steward/internal/service"
)

// P1 L5 驳回原因 CSV 导出（§6.6 技术实现第 6 条，权限 rejection.export）。

// RejectionExporter 是驳回导出能力，service.RejectionExportService 实现。
type RejectionExporter interface {
	Export(ctx context.Context, scope model.Scope, from, to time.Time) ([]model.RejectionReason, error)
}

// RejectionRoutes 挂载 GET /rejections。
func RejectionRoutes(e RejectionExporter) Mount {
	return func(farms *gin.RouterGroup) {
		farms.GET("/rejections", RejectionExportHandler(e))
	}
}

// csvBOM 是 UTF-8 BOM（表格软件据此识别中文）。
const csvBOM = "\xef\xbb\xbf"

// rejectionCSVHeader 是导出列（英文列名，顺序固定）。
var rejectionCSVHeader = []string{"id", "ticket_id", "order_id", "advice_id", "run_id", "reason_code", "reason_text",
	"operator_id", "operator_role", "created_at"}

// RejectionExportHandler 是 GET /api/v1/farms/:farm_id/rejections?from=&to=（RFC3339，必填）：text/csv（UTF-8 带 BOM，
// 便于表格软件识别中文）。时间窗缺失 / 非法 / 超过跨度上限 → 400。
func RejectionExportHandler(e RejectionExporter) gin.HandlerFunc {
	return func(c *gin.Context) {
		scope, ok := service.ScopeFrom(c.Request.Context())
		if !ok {
			abort(c, http.StatusUnauthorized, "unauthenticated")
			return
		}
		from, errFrom := time.Parse(time.RFC3339, c.Query("from"))
		to, errTo := time.Parse(time.RFC3339, c.Query("to"))
		if errFrom != nil || errTo != nil {
			abort(c, http.StatusBadRequest, "from and to required (RFC3339)")
			return
		}
		rows, err := e.Export(c.Request.Context(), scope, from, to)
		if err != nil {
			abortErr(c, err)
			return
		}
		var b strings.Builder
		b.WriteString(csvBOM)
		w := csv.NewWriter(&b)
		records := make([][]string, 0, len(rows)+1)
		records = append(records, rejectionCSVHeader)
		for _, r := range rows {
			records = append(records, rejectionRecord(r))
		}
		if err := w.WriteAll(records); err != nil {
			abortInternal(c, "rejection export: encode csv", err)
			return
		}
		name := "rejections-" + strconv.FormatUint(scope.FarmID, 10) + ".csv"
		c.Header("Content-Disposition", `attachment; filename="`+name+`"`)
		c.Data(http.StatusOK, "text/csv; charset=utf-8", []byte(b.String()))
	}
}

// rejectionRecord 把一行驳回原因转为 CSV 字段；文本列经 csvSafe 防公式注入。
func rejectionRecord(r model.RejectionReason) []string {
	text := ""
	if r.ReasonText != nil {
		text = *r.ReasonText
	}
	u := func(v uint64) string { return strconv.FormatUint(v, 10) }
	return []string{u(r.ID), u(r.TicketID), u(r.OrderID), u(r.AdviceID), u(r.RunID), csvSafe(string(r.ReasonCode)),
		csvSafe(text), u(r.OperatorID), csvSafe(string(r.OperatorRole)), r.CreatedAt.UTC().Format(time.RFC3339)}
}

// csvFormulaLead 是表格软件会当公式起始的字符（含全角 ＝ ＋ － ＠，部分软件按半角处理）。
const csvFormulaLead = "=+-@＝＋－＠"

// csvSafe 防 CSV 公式注入：以制表符 / 回车开头，或去掉前导空白（含全角空格）后以 csvFormulaLead 开头的单元格前加 '，
// 表格软件按文本显示；引号与换行由 encoding/csv 转义。
func csvSafe(s string) string {
	if s == "" {
		return s
	}
	if s[0] == '\t' || s[0] == '\r' {
		return "'" + s
	}
	if r, _ := utf8.DecodeRuneInString(strings.TrimLeftFunc(s, unicode.IsSpace)); strings.ContainsRune(csvFormulaLead, r) {
		return "'" + s
	}
	return s
}
