package report

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"path"
	"sort"

	"agri-steward/internal/evalkit"
	"agri-steward/internal/evalkit/reduce"
	"agri-steward/internal/model"
)

// P2 L3 下钻页（路线 §6.4 功能任务 6、验收 3）：{experiment_id}/report.html 每个样本一个锚点 sample-{id}，
// 列出各 trial 的 run_id、状态与 transcript 对象键。日报离线单元格链到这里（第 1 次点击），锚点处即见 run_id（第 2 次点击前）。
// 只含 ID、状态与打分器名，不含问题与输出正文（holdout 也可公开）。
const (
	// DrilldownFile 是下钻页文件名。
	DrilldownFile        = "report.html"
	drilldownContentType = "text/html; charset=utf-8"
)

// DrilldownKey 是下钻页对象键 {experiment_id}/report.html。
func DrilldownKey(expID string) string { return path.Join(expID, DrilldownFile) }

type drillTrial struct {
	Index      int
	RunID      uint64
	Status     string
	Failed     []string
	Transcript string
}

type drillSample struct {
	Anchor string
	Key    string
	Trials []drillTrial
}

var drilldownTmpl = template.Must(template.New("drill").Parse(`<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>评测下钻 {{.ID}}</title>
<style>body{font-family:sans-serif;margin:16px;background:#fff;color:#222}table{border-collapse:collapse}td,th{border:1px solid #ccc;padding:2px 6px}
section:target{background:#fff6d5}</style></head><body>
<h1>实验 {{.ID}} 下钻</h1>
<p>每个样本列出各 trial 的 run_id 与状态；只含 ID 与打分器名。</p>
{{range .Samples}}<section id="{{.Anchor}}"><h2>{{.Key}}</h2>
<table><tr><th>trial</th><th>run_id</th><th>状态</th><th>未过打分器</th><th>transcript</th></tr>
{{range .Trials}}<tr><td>{{.Index}}</td><td id="run-{{.RunID}}">{{.RunID}}</td><td>{{.Status}}</td><td>{{range .Failed}}{{.}} {{end}}</td><td>{{.Transcript}}</td></tr>
{{end}}</table></section>
{{end}}</body></html>
`))

// RenderDrilldown 渲染下钻页（按 sample_id@version 排序）。
func RenderDrilldown(exp *evalkit.Experiment) ([]byte, error) {
	groups := reduce.GroupBySample(exp.Trials)
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	samples := make([]drillSample, 0, len(keys))
	for _, k := range keys {
		ds := drillSample{Anchor: model.SampleAnchor(k), Key: k}
		for _, t := range groups[k] {
			ds.Trials = append(ds.Trials, drillTrial{Index: t.TrialIndex, RunID: t.RunID, Status: t.Status(),
				Failed: failedGraders(t), Transcript: t.Transcript})
		}
		samples = append(samples, ds)
	}
	var b bytes.Buffer
	if err := drilldownTmpl.Execute(&b, struct {
		ID      string
		Samples []drillSample
	}{exp.ID, samples}); err != nil {
		return nil, fmt.Errorf("report: render drilldown: %w", err)
	}
	return b.Bytes(), nil
}

func failedGraders(t evalkit.Trial) []string {
	var out []string
	for _, sc := range t.Scores {
		if !sc.Pass || sc.Error != "" {
			out = append(out, sc.Name)
		}
	}
	return out
}

// UploadDrilldown 渲染并上传下钻页，返回对象键。exp 用公开（holdout 脱敏后）的副本即可：只取 ID、状态与打分器名。
func UploadDrilldown(ctx context.Context, obj ObjectPutter, bucket string, exp *evalkit.Experiment) (string, error) {
	body, err := RenderDrilldown(exp)
	if err != nil {
		return "", err
	}
	key := DrilldownKey(exp.ID)
	if err := obj.PutObject(ctx, bucket, key, body, drilldownContentType); err != nil {
		return "", fmt.Errorf("report: upload %s: %w", key, err)
	}
	return key, nil
}
