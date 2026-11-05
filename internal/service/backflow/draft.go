package backflow

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"agri-steward/internal/model"
	"agri-steward/internal/piiclean"
)

// 草稿 YAML 与 internal/evalkit/sample.Sample 字段对齐（生产代码不 import evalkit，这里按同名键写；
// cmd/eval backflow import 用 sample.Parse 严格解码，字段漂移会在入库前失败）。expect / context.role 留空待专家。

// 草稿元数据。
const (
	draftSuite   = "capability"
	draftAuthor  = "backflow-draft"
	draftPending = "pending_expert"
)

var draftTags = []string{"L8", "backflow"}

type draftDoc struct {
	ID           string         `yaml:"id"`
	Version      int            `yaml:"version"`
	Suite        string         `yaml:"suite"`
	Crop         string         `yaml:"crop,omitempty"`
	Tags         []string       `yaml:"tags"`
	Question     string         `yaml:"question"`
	Context      draftContext   `yaml:"context"`
	Fixtures     draftFixtures  `yaml:"fixtures"`
	KnowledgeRef string         `yaml:"knowledge_ref"`
	Expect       map[string]any `yaml:"expect"`
	Author       string         `yaml:"author"`
	ReviewStatus string         `yaml:"review_status"`
	SignedAt     string         `yaml:"signed_at"`
	Notes        string         `yaml:"notes,omitempty"`
}

type draftContext struct {
	Farm  string `yaml:"farm"`
	Field string `yaml:"field"`
	Role  string `yaml:"role"`
	User  string `yaml:"user"`
}

type draftFixtures struct {
	Clock string                    `yaml:"clock"`
	MCP   map[string][]draftFixture `yaml:"mcp,omitempty"`
}

// draftFixture 是一个工具的桩：同一 run 只调一次用 return，多次按调用次序用 returns。
type draftFixture struct {
	Match   map[string]any `yaml:"match"`
	Return  any            `yaml:"return,omitempty"`
	Returns []any          `yaml:"returns,omitempty"`
}

// rawResult 是 TOOL_RAW 对象（mcp.Result 的 JSON：data / source / data_freshness_sec / collected_at）。
type rawResult struct {
	Data        json.RawMessage `json:"data"`
	Source      string          `json:"source"`
	CollectedAt string          `json:"collected_at"`
}

// fixtureReturn 把一份原文脱敏成 fixtures.mcp 的 return：data 为对象时并入 source / collected_at（FixtureMCP 由此
// 还原数据源与新鲜度），否则原样作为 return。
func (s *scrubber) fixtureReturn(body []byte) (any, error) {
	var r rawResult
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("decode tool raw: %w", err)
	}
	var data any
	if len(r.Data) > 0 {
		dec := json.NewDecoder(bytes.NewReader(r.Data))
		dec.UseNumber()
		if err := dec.Decode(&data); err != nil {
			return nil, fmt.Errorf("decode tool raw data: %w", err)
		}
	}
	out, err := s.value(data, false)
	if err != nil {
		return nil, err
	}
	m, ok := out.(map[string]any)
	if !ok {
		return out, nil
	}
	if r.Source != "" {
		if m["source"], err = s.text(r.Source); err != nil {
			return nil, err
		}
	}
	if r.CollectedAt != "" {
		m["collected_at"] = r.CollectedAt
	}
	return m, nil
}

// toolReturn 是一次工具调用脱敏后的返回。
type toolReturn struct {
	name string
	ret  any
}

// draftInput 是组装一份草稿所需的脱敏后材料。
type draftInput struct {
	ref, question, crop, kbRef string
	clock                      time.Time
	tools                      []toolReturn
	skipped                    []string // 原文过保留期 / 无法解析而跳过的工具
}

// buildDraft 组装草稿（match 用 context.field 的合成码；同名工具按调用次序合并为 returns）。
func buildDraft(s *scrubber, in draftInput) draftDoc {
	byTool := map[string][]any{}
	var order []string
	for _, t := range in.tools {
		if _, ok := byTool[t.name]; !ok {
			order = append(order, t.name)
		}
		byTool[t.name] = append(byTool[t.name], t.ret)
	}
	mcp := make(map[string][]draftFixture, len(order))
	for _, name := range order {
		f := draftFixture{Match: map[string]any{"field": s.field}}
		if rets := byTool[name]; len(rets) == 1 {
			f.Return = rets[0]
		} else {
			f.Returns = rets
		}
		mcp[name] = []draftFixture{f}
	}
	notes := "P2 L8 线上回流草稿：expect 与 context.role 待专家补写并署名后进 capability"
	if len(in.skipped) > 0 {
		notes += "；以下工具原文过保留期或无法解析，未生成桩：" + strings.Join(in.skipped, "、")
	}
	return draftDoc{ID: in.ref, Version: 1, Suite: draftSuite, Crop: in.crop, Tags: slices.Clone(draftTags),
		Question: in.question, Context: draftContext{Farm: s.farm, Field: s.field, User: s.user},
		Fixtures: draftFixtures{Clock: in.clock.Format(time.RFC3339), MCP: mcp}, KnowledgeRef: in.kbRef,
		Expect: map[string]any{}, Author: draftAuthor, ReviewStatus: draftPending, Notes: notes}
}

// gateLeak 是脱敏后仍需拦下的残留：pii_clean 命中，或真实 ID 原样出现。
type gateLeak struct{ where, kind string }

// realIDMinDigits 以下的真实 ID 不做原样出现检查（1、2 位数在数值里太常见，误判会让每份草稿都 PII_FAILED）。
// 评估过调到 5，暂不调。3、4 位确有误报：草稿 fixtures.clock 含年份 2026，桩返回有风向 0-360、气压约 1013 等读数，
// ID 恰好等于这些数时该 run 置 PII_FAILED。但误报的代价只是丢一份样本（失败关闭，不泄露）。调到 5 则 3、4 位的
// farm / field / user ID（种子数据与上线初期的规模）藏在非 ID 键下时兜底扫描拦不住，属脱敏红线，代价更高。
// 待线上数据：按 PII_FAILED 中 kind=real_id 的占比再定。
const realIDMinDigits = 3

var digitRunRe = regexp.MustCompile(`[0-9]+`)

// gate 是写出前的兜底扫描：每个桩返回过 piiclean.FirstLeak（JSON 键 / 值、坐标键精度），YAML 逐行过 piiclean.Detect
// （与 sample 文件扫描同一口径），再查真实 farm / field / user / run ID 是否作为完整数字串残留。
func gate(doc draftDoc, out []byte, realIDs []uint64) (gateLeak, bool) {
	for tool, fs := range doc.Fixtures.MCP {
		for _, f := range fs {
			for _, ret := range append([]any{f.Return}, f.Returns...) {
				if ret == nil {
					continue
				}
				b, err := json.Marshal(ret)
				if err != nil {
					return gateLeak{where: "fixtures.mcp." + tool, kind: "unencodable"}, true
				}
				if key, l, ok := piiclean.FirstLeak(string(b)); ok {
					return gateLeak{where: "fixtures.mcp." + tool + "." + key, kind: l.Kind}, true
				}
			}
		}
	}
	for i, line := range strings.Split(string(out), "\n") {
		if l, ok := piiclean.Detect(line); ok {
			return gateLeak{where: "line " + strconv.Itoa(i+1), kind: l.Kind}, true
		}
	}
	real := map[string]bool{}
	for _, id := range realIDs {
		if s := strconv.FormatUint(id, 10); len(s) >= realIDMinDigits {
			real[s] = true
		}
	}
	for _, d := range digitRunRe.FindAllString(string(out), -1) {
		if real[d] {
			return gateLeak{where: "yaml", kind: "real_id"}, true
		}
	}
	return gateLeak{}, false
}

// encodeDraft 把草稿编码为 YAML（两格缩进，与仓库样本一致）。
func encodeDraft(doc draftDoc) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("backflow: encode draft: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("backflow: encode draft: %w", err)
	}
	return buf.Bytes(), nil
}

// materialRealIDs 是本 run 不得出现在草稿里的真实 ID。
func materialRealIDs(tenantID, farmID uint64, m model.BackflowMaterial) []uint64 {
	return []uint64{tenantID, farmID, m.FieldID, m.UserID, m.RunID}
}
