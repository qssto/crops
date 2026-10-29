package controller

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// stewardPagePath 是管家页 JSON（web/amis 被 gitignore，文件缺失时跳过）。
var stewardPagePath = filepath.Join("..", "..", "web", "amis", StewardFile)

// stewardContract 是管家页允许调用的接口（冻结契约，参数段统一写作 :name）。
var stewardContract = []string{
	"/api/v1/farms/:farm_id/ask",
	"/api/v1/farms/:farm_id/runs/:run_id",
	"/api/v1/farms/:farm_id/runs/:run_id/events",
	"/api/v1/farms/:farm_id/runs/:run_id/raw/:raw_id",
	"/api/v1/farms/:farm_id/approvals",
	"/api/v1/farms/:farm_id/approvals/:ticket_id/decide",
}

var (
	// apiPathRe 取 /api/v1/ 路径：amis 模板 ${x} 与 JS 拼接 ' + expr + ' 都算路径参数段，遇引号 / ? / 空白结束。
	apiPathRe   = regexp.MustCompile(`/api/v1/(?:[^"'?\s]|' \+ [^']+? \+ ')*`)
	tplParamRe  = regexp.MustCompile(`\$\{[^}]*\}`)
	jsParamRe   = regexp.MustCompile(`' \+ [^']+? \+ '`)
	namedSegRe  = regexp.MustCompile(`:[A-Za-z_]+`)
	paramMarker = ":"
)

// normalizeAPIPath 把各种写法的参数段统一为 ":"，便于与契约比较。
func normalizeAPIPath(p string) string {
	p = tplParamRe.ReplaceAllString(p, paramMarker)
	p = jsParamRe.ReplaceAllString(p, paramMarker)
	return namedSegRe.ReplaceAllString(p, paramMarker)
}

func TestNormalizeAPIPath(t *testing.T) {
	tests := []struct{ in, want string }{
		{"/api/v1/farms/${farm_id}/ask", "/api/v1/farms/:/ask"},
		{"/api/v1/farms/' + encodeURIComponent(d.farm_id) + '/runs/' + x + '/events", "/api/v1/farms/:/runs/:/events"},
		{"/api/v1/farms/:farm_id/runs/:run_id", "/api/v1/farms/:/runs/:"},
		{"/api/v1/farms/1/ask", "/api/v1/farms/1/ask"},
	}
	for _, tt := range tests {
		if got := normalizeAPIPath(tt.in); got != tt.want {
			t.Errorf("normalizeAPIPath(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestStewardPage(t *testing.T) {
	raw, err := os.ReadFile(stewardPagePath)
	if errors.Is(err, fs.ErrNotExist) {
		t.Skipf("%s 未安装，跳过", stewardPagePath)
	}
	if err != nil {
		t.Fatal(err)
	}
	var page map[string]any
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatalf("steward.json 不是合法 JSON: %v", err)
	}
	if page["type"] != "page" {
		t.Fatalf("顶层 type = %v, want page", page["type"])
	}

	allowed := make(map[string]string, len(stewardContract))
	for _, p := range stewardContract {
		allowed[normalizeAPIPath(p)] = p
	}
	used := map[string]bool{}
	text := string(raw)
	for _, p := range apiPathRe.FindAllString(text, -1) {
		route, ok := allowed[normalizeAPIPath(p)]
		if !ok {
			t.Errorf("页面调用了契约外的接口 %q（归一化 %q）", p, normalizeAPIPath(p))
			continue
		}
		used[route] = true
	}
	for _, p := range stewardContract {
		if !used[p] {
			t.Errorf("页面未调用契约接口 %s", p)
		}
	}

	// 契约细节与醒目标注：审批列表只看 OPEN；SSE 用 query 带 token 与断点；决策带幂等头；dry-run 与 HIGH 二次确认标注。
	for _, marker := range []string{
		"status=OPEN", "access_token=", "last_event_id=", `"X-Request-Id"`, "snapshot_digest",
		"dry-run · 未触达真实设备", "待第二人确认", "EventSource", `addEventListener('end'`,
	} {
		if !strings.Contains(text, marker) {
			t.Errorf("steward.json 缺少 %q", marker)
		}
	}
}
