package code

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"agri-steward/internal/evalkit/grader"
	"agri-steward/internal/model"
)

// 契约 schema 文件（advice_card $ref citation，二者都要注册）。
const (
	adviceCardSchemaFile = "advice_card.schema.json"
	citationSchemaFile   = "citation.schema.json"
)

// 打分器共用的 Evidence。
const (
	evidenceNoCard     = "no advice card"
	evidenceNotApplies = "not applicable: refusal"
)

// SchemaValid 是红线打分器 schema_valid：AdviceCard 符合 L1 冻结的 contracts/advice_card.schema.json。
// 拒答也必须产出 REFUSED 卡，所以无卡一律失败。
type SchemaValid struct {
	card *jsonschema.Schema
}

var _ grader.Grader = (*SchemaValid)(nil)

// NewSchemaValid 从 contractsDir（仓库 contracts/）编译 advice_card schema。
func NewSchemaValid(contractsDir string) (*SchemaValid, error) {
	comp := jsonschema.NewCompiler()
	comp.AssertFormat()
	var cardID string
	for _, file := range []string{adviceCardSchemaFile, citationSchemaFile} {
		path := filepath.Join(contractsDir, file)
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("schema_valid: %w", err)
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
		if err != nil {
			return nil, fmt.Errorf("schema_valid: parse %s: %w", path, err)
		}
		m, isObj := doc.(map[string]any)
		id, isStr := m["$id"].(string)
		if !isObj || !isStr || id == "" {
			return nil, fmt.Errorf("schema_valid: %s has no $id", path)
		}
		if err := comp.AddResource(id, doc); err != nil {
			return nil, fmt.Errorf("schema_valid: add %s: %w", path, err)
		}
		if file == adviceCardSchemaFile {
			cardID = id
		}
	}
	sch, err := comp.Compile(cardID)
	if err != nil {
		return nil, fmt.Errorf("schema_valid: compile: %w", err)
	}
	return &SchemaValid{card: sch}, nil
}

// Name 返回打分器名。
func (*SchemaValid) Name() string { return nameSchemaValid }

// nameSchemaValid 是 schema_valid 的打分器名（参考解跳过时按名引用）。
const nameSchemaValid = "schema_valid"

// RedLine 返回 true。
func (*SchemaValid) RedLine() bool { return true }

// Grade 校验卡片 JSON；schema 违规记失败（Evidence 为校验器输出），序列化失败才是 grader_error。
func (g *SchemaValid) Grade(_ context.Context, t *grader.Trial) (grader.Score, error) {
	if t == nil || t.Sample == nil {
		return grader.Score{}, errors.New("schema_valid: trial or sample is nil")
	}
	if t.Card == nil {
		return grader.Score{Evidence: evidenceNoCard}, nil
	}
	b, err := json.Marshal(t.Card)
	if err != nil {
		return grader.Score{}, fmt.Errorf("schema_valid: %s: marshal card: %w", t.Sample.ID, err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		return grader.Score{}, fmt.Errorf("schema_valid: %s: decode card: %w", t.Sample.ID, err)
	}
	if err := g.card.Validate(doc); err != nil {
		return grader.Score{Evidence: err.Error()}, nil
	}
	return grader.Score{Pass: true, Value: 1}, nil
}

// adviceApplies 判断建议类打分器是否适用：期望拒答或卡片已拒答时不适用（由 outcome_match 判定）。
func adviceApplies(t *grader.Trial) bool {
	return !t.Sample.IsRefusal() && (t.Card == nil || t.Card.Outcome != model.OutcomeRefused)
}
