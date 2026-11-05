package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agri-steward/internal/evalkit/store"
)

// 构造草稿（无线上数据）：与 backflow-collect 写出的格式一致，只含合成码与粗化坐标。
const (
	goodRef  = "bf-abcdefghijklmnop"
	otherRef = "bf-qrstuvwxyzabcdef"
)

func draftYAML(ref, question, extra string) string {
	return "id: " + ref + `
version: 1
suite: capability
crop: rice
tags:
  - L8
  - backflow
question: ` + question + `
context:
  farm: farm-bf-abcdefgh
  field: field-bf-abcdefgh
  role: ""
  user: u-bf-abcdefgh
fixtures:
  clock: "2026-10-28T11:00:00+08:00"
  mcp:
    weather.forecast:
      - match:
          field: field-bf-abcdefgh
        return:
          lat: 31.23
          wind_speed_ms: 2
          source: mock-station
knowledge_ref: kb-rice@v3
expect: {}
author: backflow-draft
review_status: pending_expert
signed_at: ""
` + extra
}

func writeDrafts(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestBackflowDrafts_Pass：合规草稿出 BACKFLOW_EXPECT 任务（draft_ref = 文件名），点文件（.gitkeep / 临时文件）与非 yaml 跳过。
func TestBackflowDrafts_Pass(t *testing.T) {
	dir := writeDrafts(t, map[string]string{
		otherRef + ".yaml": draftYAML(otherRef, "3号田稻飞虱要不要打药？", ""),
		goodRef + ".yaml":  draftYAML(goodRef, "联系 syn-phone-abcdefgh，要不要打药？", ""),
		".gitkeep":         "",
		".draft-123.yaml":  "half",
		"README.txt":       "not a draft",
	})
	tasks, rejects, err := checkBackflowDrafts(dir)
	if err != nil || len(rejects) != 0 {
		t.Fatalf("check = %v, %v", rejects, err)
	}
	if len(tasks) != 2 || tasks[0].DraftRef != goodRef || tasks[1].DraftRef != otherRef {
		t.Fatalf("tasks = %+v", tasks)
	}
	for _, task := range tasks {
		if task.TaskType != store.TaskBackflowExpect || task.SampleID != "" || task.ExperimentID != nil {
			t.Fatalf("task = %+v, want BACKFLOW_EXPECT keyed by draft_ref only", task)
		}
	}
}

// TestBackflowDrafts_Reject（脱敏红线）：入库前再跑一次 pii_clean，手机号 / 身份证 / 精确坐标命中即拒；
// 原因不回显命中的值；格式漂移（未知字段、id 与文件名不符、元数据被改、文件名不是 draft_ref）同样拒绝。
// 样本解码失败只报类别与行号，不回显草稿原文（字段名、取值）。
func TestBackflowDrafts_Reject(t *testing.T) {
	cases := map[string]struct{ name, body, want string }{
		"phone":   {goodRef + ".yaml", draftYAML(goodRef, "我手机13812345678要不要打药？", ""), "pii_clean hit (phone)"},
		"id_card": {goodRef + ".yaml", draftYAML(goodRef, "身份证110101199003071234", ""), "pii_clean hit"},
		"coord":   {goodRef + ".yaml", draftYAML(goodRef, "北纬31.234567 东经121.456789", ""), "pii_clean hit"},
		"unknown": {goodRef + ".yaml", draftYAML(goodRef, "要不要打药？", "farm_id_real: x\n"),
			"not a sample: unknown field at line 29"},
		"type_mismatch": {goodRef + ".yaml", strings.Replace(draftYAML(goodRef, "要不要打药？", ""),
			"version: 1", "version: rawsecret42", 1), "not a sample: type mismatch at line 2"},
		"syntax": {goodRef + ".yaml", draftYAML(goodRef, "要不要打药？", "rawsecret43: [unclosed\n"),
			"not a sample: syntax error at line"},
		"empty":       {goodRef + ".yaml", "", "not a sample: empty"},
		"id_mismatch": {goodRef + ".yaml", draftYAML(otherRef, "要不要打药？", ""), "id does not match"},
		"signed": {goodRef + ".yaml", strings.Replace(draftYAML(goodRef, "要不要打药？", ""),
			"review_status: pending_expert", "review_status: approved", 1), "review_status"},
		"bad_name":   {"run-990011.yaml", draftYAML("run-990011", "要不要打药？", ""), "not a draft_ref"},
		"digit_name": {"bf-abcdefgh12345678.yaml", draftYAML("bf-abcdefgh12345678", "要不要打药？", ""), "not a draft_ref"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			dir := writeDrafts(t, map[string]string{c.name: c.body, otherRef + ".yaml": draftYAML(otherRef, "要不要打药？", "")})
			tasks, rejects, err := checkBackflowDrafts(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(rejects) != 1 || !strings.Contains(rejects[0], c.want) || len(tasks) != 1 {
				t.Fatalf("rejects = %v tasks = %+v, want one %q", rejects, tasks, c.want)
			}
			for _, leak := range []string{"13812345678", "138", "110101199003071234", "31.234567", "farm_id_real", "rawsecret"} {
				if strings.Contains(rejects[0], leak) {
					t.Fatalf("reject reason echoes %q: %s", leak, rejects[0])
				}
			}
		})
	}
}

// TestBackflowImport_RejectsWholeBatch：有一份草稿不过就整批不入库（不连评测库，退出非 0）。
func TestBackflowImport_RejectsWholeBatch(t *testing.T) {
	dir := writeDrafts(t, map[string]string{
		goodRef + ".yaml":  draftYAML(goodRef, "要不要打药？", ""),
		otherRef + ".yaml": draftYAML(otherRef, "我手机13812345678", ""),
	})
	var out, errOut strings.Builder
	// --config 指向不存在的文件：若误连库会报配置错误而不是 ErrBackflowDraft。
	err := cmdBackflowImport(context.Background(), []string{"--dir", dir, "--config", filepath.Join(dir, "missing.yaml")},
		&out, &errOut, nil)
	if !errors.Is(err, ErrBackflowDraft) {
		t.Fatalf("err = %v, want ErrBackflowDraft", err)
	}
	if !strings.Contains(out.String(), "drafts=2 rejected=1 inserted=0") || !strings.Contains(errOut.String(), otherRef) {
		t.Fatalf("stdout = %q stderr = %q", out.String(), errOut.String())
	}
}

type fakeInserter struct {
	got []store.ReviewTask
	at  time.Time
	err error
}

func (f *fakeInserter) Insert(_ context.Context, tasks []store.ReviewTask, at time.Time) (int, error) {
	f.got, f.at = append(f.got, tasks...), at
	return len(tasks), f.err
}

// TestInsertBackflowTasks：空批不碰库；写库错误包装返回。
func TestInsertBackflowTasks(t *testing.T) {
	at := time.Date(2026, 11, 2, 2, 0, 0, 0, time.UTC)
	f := &fakeInserter{}
	if n, err := insertBackflowTasks(context.Background(), f, nil, at); n != 0 || err != nil || f.got != nil {
		t.Fatalf("empty = %d, %v, %v", n, err, f.got)
	}
	tasks := []store.ReviewTask{{TaskType: store.TaskBackflowExpect, DraftRef: goodRef}}
	if n, err := insertBackflowTasks(context.Background(), f, tasks, at); n != 1 || err != nil || !f.at.Equal(at) {
		t.Fatalf("insert = %d, %v", n, err)
	}
	boom := errors.New("boom")
	f.err = boom
	if _, err := insertBackflowTasks(context.Background(), f, tasks, at); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want wrapped boom", err)
	}
}

// TestBackflow_Usage：缺子命令 / 未知子命令报用法（与 judge 同口径，退出码 exitFailed）。
func TestBackflow_Usage(t *testing.T) {
	for _, args := range [][]string{{"backflow"}, {"backflow", "merge"}} {
		if _, errOut, code := cliUnit(t, args...); code != exitFailed || !strings.Contains(errOut, "eval backflow import") {
			t.Errorf("%v exit = %d stderr = %q", args, code, errOut)
		}
	}
}
