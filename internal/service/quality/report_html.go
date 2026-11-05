package quality

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"sort"

	"agri-steward/internal/model"
	"agri-steward/internal/service/quality/stat"
)

// sliceRows 是离线非 all 切片：与基线同切片做差；可检验的切片一起做 Holm 校正，显著且变差的标红。
func sliceRows(in ReportInput, cur, base map[sliceKey]model.OfflineMetric) []SliceRow {
	if in.Offline.State != OfflineOK || in.Offline.Metrics == nil {
		return nil
	}
	var rows []SliceRow
	var ps []float64
	var tested []int // rows 下标，与 ps 同序
	worse := map[int]bool{}
	for _, x := range in.Offline.Metrics.Metrics {
		if x.Slice == model.SliceAll {
			continue
		}
		title := x.Title
		if title == "" {
			title = metricTitles[x.Metric]
		}
		r := SliceRow{Metric: x.Metric, Title: title, Slice: x.Slice,
			Cell: offlineCell(in, cur, base, x.Metric, x.Slice), P: TextNoDiff}
		b, ok := base[sliceKey{x.Metric, x.Slice}]
		if r.Cell.Value && ok && b.Status == model.MetricStatusOK && !uncalibrated(x, b) {
			p := diffP(x, b)
			r.P = fmt.Sprintf("%.3f", p)
			worse[len(rows)] = (x.Point < b.Point) != x.LowerIsBetter && x.Point != b.Point
			tested = append(tested, len(rows))
			ps = append(ps, p)
		}
		rows = append(rows, r)
	}
	for i, sig := range stat.Holm(ps, HolmAlpha) {
		if idx := tested[i]; sig && worse[idx] {
			rows[idx].Cell.Red = true
		}
	}
	sort.SliceStable(rows, func(a, b int) bool {
		if rows[a].Metric != rows[b].Metric {
			return rows[a].Metric < rows[b].Metric
		}
		return rows[a].Slice < rows[b].Slice
	})
	return rows
}

var dailyTmpl = template.Must(template.New("daily").Parse(`<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>可信度日报 {{.Date}}</title>
<style>
:root{--fg:#1f2328;--bg:#fff;--muted:#59636e;--line:#d1d9e0;--red:#cf222e}
@media (prefers-color-scheme: dark){:root{--fg:#e6edf3;--bg:#0d1117;--muted:#9198a1;--line:#3d444d;--red:#ff7b72}}
body{font:14px/1.5 system-ui,sans-serif;color:var(--fg);background:var(--bg);margin:16px}
table{border-collapse:collapse;margin:8px 0 24px;max-width:100%}th,td{border:1px solid var(--line);padding:4px 8px;text-align:left;vertical-align:top}
.muted{color:var(--muted)}.red{color:var(--red);font-weight:600}a{color:inherit}
</style></head><body>
<h1>可信度日报 {{.Date}}</h1>
<p class="muted">离线：{{if eq .OfflineState "OK"}}实验 <a href="{{.ReportHref}}">{{.ExperimentID}}</a>，bootstrap {{.Resamples}} 次，seed {{.Seed}}{{else if eq .OfflineState "VOID"}}实验 {{.ExperimentID}} 本期作废（grader_error）{{else}}无数据{{end}}。
基线：{{if .BaselineName}}{{.BaselineName}}{{else}}未配置{{end}}。门禁只看 all 切片；切片差值只出报告，标红前做 Holm 校正（α=0.05）。</p>
<h2>all 切片</h2>
<table><tr><th>指标</th><th>离线</th><th>区间</th><th>相对基线</th><th>线上</th><th>区间</th><th>相对基线</th><th>下钻 run</th></tr>
{{range .Rows}}<tr id="metric-{{.Metric}}"><td>{{.Title}}<br><span class="muted">{{.Metric}}</span></td>
<td>{{template "cell" .Offline}}</td><td>{{.Offline.CI}}</td><td>{{.Offline.Diff}}</td>
<td>{{template "cell" .Online}}</td><td>{{.Online.CI}}</td><td>{{.Online.Diff}}</td>
<td>{{range .Online.Runs}}<a href="{{.Href}}">{{.RunID}}</a> {{end}}</td></tr>
{{end}}</table>
<h2>离线切片</h2>
{{if .Slices}}<table><tr><th>指标</th><th>切片</th><th>值</th><th>区间</th><th>相对基线</th><th>p</th></tr>
{{range .Slices}}<tr><td>{{.Title}}</td><td>{{.Slice}}</td><td{{if .Cell.Red}} class="red"{{end}}>{{template "cell" .Cell}}</td>
<td>{{.Cell.CI}}</td><td{{if .Cell.Red}} class="red"{{end}}>{{.Cell.Diff}}</td><td>{{.P}}</td></tr>
{{end}}</table>{{else}}<p class="muted">无切片数据。</p>{{end}}
</body></html>
{{define "cell"}}{{if .Link}}<a href="{{.Link}}">{{.Text}}</a>{{else}}{{.Text}}{{end}}{{if .Note}} <span class="muted">{{.Note}}</span>{{end}}{{end}}`))

// RenderReport 渲染日报 HTML。
func RenderReport(rep Report) ([]byte, error) {
	var b bytes.Buffer
	if err := dailyTmpl.Execute(&b, rep); err != nil {
		return nil, fmt.Errorf("quality: render daily report %s: %w", rep.Date, err)
	}
	return b.Bytes(), nil
}

// UploadReport 渲染并写 eval 桶 report/daily/{yyyymmdd}.html，返回对象键。
func UploadReport(ctx context.Context, obj Objects, bucket string, rep Report) (string, error) {
	body, err := RenderReport(rep)
	if err != nil {
		return "", err
	}
	key := model.DailyReportKey(rep.Date)
	if err := obj.PutObject(ctx, bucket, key, body, "text/html; charset=utf-8"); err != nil {
		return "", fmt.Errorf("quality: upload %s: %w", key, err)
	}
	return key, nil
}
