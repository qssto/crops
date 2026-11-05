package attribution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"slices"

	"agri-steward/internal/model"
)

// LoadGaps 是 L5 kb_load_gaps.json（evalkit 写；业务侧自定义结构只读用到的字段，不 import evalkit）。
type LoadGaps struct {
	KBMiss []struct {
		SampleID     string `json:"sample_id"`
		QuestionType string `json:"question_type"`
		Topic        string `json:"topic"`
		Trials       int    `json:"trials"`
	} `json:"kb_miss"`
	LowHit []struct {
		Topic   string   `json:"topic"`
		HitRate float64  `json:"hit_rate"`
		Missing []string `json:"missing"`
	} `json:"low_hit"`
}

// Report 是周报模板数据。
type Report struct {
	Week         string
	Top          []model.AttributionIssue
	Previous     []Backfill // 上期 Top 问题的处置回填
	PreviousWeek string
	HasPrevious  bool
	Reopened     []model.AttributionIssue
	Backflow     []StatusCount // 本周推进到各状态的 run 数（一个 run 同周 SELECTED→DRAFTED 两栏都计）
	Selected     int           // 本周新选入回流的 run 数
	Merged       int
	Gaps         *LoadGaps // nil 表示无
	ExpertWired  bool
}

// Backfill 是上期一个 Top 问题：当时的状态与现在的处置。
type Backfill struct {
	Then SnapshotIssue
	Now  *model.AttributionIssue // nil：行已不存在
}

// StatusCount 是一个回流状态的 run 数。
type StatusCount struct {
	Status string
	N      int
}

// backflowStatuses 是周报回流计数的列出顺序。
var backflowStatuses = []string{model.BackflowSelected, model.BackflowDrafted, model.BackflowPIIFailed,
	model.BackflowFailed, model.BackflowMerged, model.BackflowDropped}

// report 组装并上传周报，最后写 done 标记（标记写失败则下周期重出一次，周报同键覆盖）。
func (j *Job) report(ctx context.Context, w Week, flow map[string]int, out *Outcome) error {
	top, err := j.d.Issues.WeekIssues(ctx, j.d.System, w.Name, j.d.TopN)
	if err != nil {
		return fmt.Errorf("attribution: top issues %s: %w", w.Name, err)
	}
	rep := Report{Week: w.Name, Top: top, ExpertWired: j.d.Experts != nil, Merged: flow[model.BackflowMerged]}
	for _, s := range backflowStatuses {
		rep.Backflow = append(rep.Backflow, StatusCount{Status: s, N: flow[s]})
	}
	rep.Selected = flow[model.BackflowSelected]
	if err := j.previous(ctx, w, &rep); err != nil {
		return err
	}
	// 补跑时本周重开已在首次写入 reopen_count，沿用首次快照里的重开签名。
	if cur, ok, err := j.snapshot(ctx, w.Name); err != nil {
		return err
	} else if ok {
		out.Reopened = mergeSigs(out.Reopened, cur.Reopened)
	}
	reopened, err := j.d.Issues.IssuesBySignature(ctx, j.d.System, out.Reopened)
	if err != nil {
		return fmt.Errorf("attribution: reopened issues %s: %w", w.Name, err)
	}
	rep.Reopened = reopened
	if rep.Gaps, err = j.loadGaps(ctx); err != nil {
		return err
	}
	body, err := RenderReport(rep)
	if err != nil {
		return err
	}
	key := ReportKey(w.Name)
	if err := j.d.Objects.PutObject(ctx, j.d.Bucket, key, body, "text/html; charset=utf-8"); err != nil {
		return fmt.Errorf("attribution: upload %s: %w", key, err)
	}
	out.ReportKey = key
	return j.markDone(ctx, w.Name, key, top, out.Reopened)
}

// previous 读上一期 done 快照，按签名回填上期 Top 问题现在的负责人 / 处置 / 状态。
func (j *Job) previous(ctx context.Context, w Week, rep *Report) error {
	prev := PrevWeek(w.From, j.d.Loc).Name
	rep.PreviousWeek = prev
	snap, ok, err := j.snapshot(ctx, prev)
	if err != nil || !ok {
		return err
	}
	rep.HasPrevious = true
	sigs := make([]string, len(snap.Top))
	for i, s := range snap.Top {
		sigs[i] = s.Signature
	}
	now, err := j.d.Issues.IssuesBySignature(ctx, j.d.System, sigs)
	if err != nil {
		return fmt.Errorf("attribution: backfill issues %s: %w", prev, err)
	}
	bySig := make(map[string]*model.AttributionIssue, len(now))
	for i := range now {
		bySig[now[i].Signature] = &now[i]
	}
	for _, s := range snap.Top {
		rep.Previous = append(rep.Previous, Backfill{Then: s, Now: bySig[s.Signature]})
	}
	return nil
}

// loadGaps 读 eval 桶里 L5 的 kb_load_gaps.json；未配置或对象不存在返回 nil（周报写「无」）。
func (j *Job) loadGaps(ctx context.Context) (*LoadGaps, error) {
	if j.d.KBLoadGapsKey == "" {
		return nil, nil
	}
	raw, err := j.d.Objects.GetObject(ctx, j.d.Bucket, j.d.KBLoadGapsKey)
	switch {
	case errors.Is(err, model.ErrNotFound):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("attribution: read %s: %w", j.d.KBLoadGapsKey, err)
	}
	var g LoadGaps
	if err := json.Unmarshal(raw, &g); err != nil {
		return nil, fmt.Errorf("attribution: decode %s: %w", j.d.KBLoadGapsKey, err)
	}
	return &g, nil
}

func (j *Job) markDone(ctx context.Context, week, key string, top []model.AttributionIssue, reopened []string) error {
	s := Snapshot{Week: week, ReportKey: key, GeneratedAt: j.d.Clock.Now().UTC(), Reopened: reopened}
	for _, i := range top {
		s.Top = append(s.Top, SnapshotIssue{ID: i.ID, Signature: i.Signature, HitCount: i.HitCount, Status: i.Status,
			Disposition: i.Disposition, OwnerID: i.OwnerID})
	}
	body, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("attribution: marshal done marker %s: %w", week, err)
	}
	if err := j.d.Objects.PutObject(ctx, j.d.Bucket, DoneKey(week), body, "application/json"); err != nil {
		return fmt.Errorf("attribution: write done marker %s: %w", week, err)
	}
	return nil
}

// mergeSigs 合并两组签名（去重、升序）。
func mergeSigs(a, b []string) []string {
	out := slices.Concat(a, b)
	slices.Sort(out)
	return slices.Compact(out)
}

// deref 让模板显示可空字段（nil 显示「—」）。
func deref(v any) string {
	switch p := v.(type) {
	case *string:
		if p != nil {
			return *p
		}
	case *uint64:
		if p != nil {
			return fmt.Sprint(*p)
		}
	}
	return "—"
}

var weeklyTmpl = template.Must(template.New("weekly").Funcs(template.FuncMap{"d": deref}).Parse(`<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>归因周报 {{.Week}}</title>
<style>
:root{--fg:#1f2328;--bg:#fff;--muted:#59636e;--line:#d1d9e0}
@media (prefers-color-scheme: dark){:root{--fg:#e6edf3;--bg:#0d1117;--muted:#9198a1;--line:#3d444d}}
body{font:14px/1.5 system-ui,sans-serif;color:var(--fg);background:var(--bg);margin:16px}
table{border-collapse:collapse;margin:8px 0 24px;max-width:100%}th,td{border:1px solid var(--line);padding:4px 8px;text-align:left;vertical-align:top}
.muted{color:var(--muted)}
</style></head><body>
<h1>归因周报 {{.Week}}</h1>
<p class="muted">按「原因 × 问题类型 × 主 kb_id」聚合上周驳回、差评、护栏拦截（GUARDRAIL）、KB_MISS 拒答与专家判错。
{{if not .ExpertWired}}评测库 review_task 专家判错未接入（待调度方决定 server 连评测库）。{{end}}</p>
<h2>Top 问题</h2>
{{if .Top}}<table><tr><th>#</th><th>原因</th><th>问题类型</th><th>主 kb</th><th>本周命中</th><th>首见</th><th>负责人</th><th>处置</th><th>状态</th><th>重开</th></tr>
{{range $i, $x := .Top}}<tr><td>{{$x.ID}}</td><td>{{$x.Reason}}</td><td>{{$x.QuestionType}}</td><td>{{d $x.PrimaryKB}}</td><td>{{$x.HitCount}}</td>
<td>{{$x.FirstWeek}}</td><td>{{d $x.OwnerID}}</td><td>{{d $x.Disposition}}</td><td>{{$x.Status}}</td><td>{{$x.ReopenCount}}</td></tr>
{{end}}</table>{{else}}<p class="muted">本周无负向信号。</p>{{end}}
<h2>上期处置回填（{{.PreviousWeek}}）</h2>
{{if .Previous}}<table><tr><th>#</th><th>上期命中</th><th>上期状态</th><th>负责人</th><th>处置</th><th>现状态</th><th>处置记录</th></tr>
{{range .Previous}}<tr><td>{{.Then.ID}}</td><td>{{.Then.HitCount}}</td><td>{{.Then.Status}}</td>
{{if .Now}}<td>{{d .Now.OwnerID}}</td><td>{{d .Now.Disposition}}</td><td>{{.Now.Status}}</td><td>{{d .Now.ResolutionRef}}</td>{{else}}<td colspan="4" class="muted">行已不存在</td>{{end}}</tr>
{{end}}</table>{{else if .HasPrevious}}<p class="muted">上期无 Top 问题。</p>{{else}}<p class="muted">无上期周报。</p>{{end}}
<h2>重开问题</h2>
{{if .Reopened}}<table><tr><th>#</th><th>原因</th><th>问题类型</th><th>主 kb</th><th>本周命中</th><th>重开次数</th><th>上次处置</th><th>处置记录</th></tr>
{{range .Reopened}}<tr><td>{{.ID}}</td><td>{{.Reason}}</td><td>{{.QuestionType}}</td><td>{{d .PrimaryKB}}</td><td>{{.HitCount}}</td>
<td>{{.ReopenCount}}</td><td>{{d .Disposition}}</td><td>{{d .ResolutionRef}}</td></tr>
{{end}}</table>{{else}}<p class="muted">本周无重开。</p>{{end}}
<h2>线上回流</h2>
<p>本周选入回流 {{.Selected}} 个 run，入库 {{.Merged}} 个（MERGED 待专家补 expect）。</p>
<table><tr>{{range .Backflow}}<th>{{.Status}}</th>{{end}}</tr><tr>{{range .Backflow}}<td>{{.N}}</td>{{end}}</tr></table>
<h2>离线知识加载缺口（kb_load_gaps.json）</h2>
{{with .Gaps}}{{if .KBMiss}}<h3>KB_MISS</h3><table><tr><th>样本</th><th>问题类型</th><th>主题</th><th>trial</th></tr>
{{range .KBMiss}}<tr><td>{{.SampleID}}</td><td>{{.QuestionType}}</td><td>{{.Topic}}</td><td>{{.Trials}}</td></tr>{{end}}</table>{{end}}
{{if .LowHit}}<h3>低命中切片</h3><table><tr><th>主题</th><th>命中率</th><th>未加载 must_cite</th></tr>
{{range .LowHit}}<tr><td>{{.Topic}}</td><td>{{printf "%.2f" .HitRate}}</td><td>{{range .Missing}}{{.}} {{end}}</td></tr>{{end}}</table>{{end}}
{{if not (or .KBMiss .LowHit)}}<p class="muted">无</p>{{end}}{{else}}<p class="muted">无</p>{{end}}
</body></html>`))

// RenderReport 渲染周报 HTML。
func RenderReport(rep Report) ([]byte, error) {
	var b bytes.Buffer
	if err := weeklyTmpl.Execute(&b, rep); err != nil {
		return nil, fmt.Errorf("attribution: render weekly report %s: %w", rep.Week, err)
	}
	return b.Bytes(), nil
}
