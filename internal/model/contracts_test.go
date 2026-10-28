package model_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"agri-steward/internal/model"
)

const contractsDir = "../../contracts"

// contracts 是五个契约与对应 schema 文件。
var contracts = []struct {
	file string
	typ  reflect.Type
}{
	{"advice_card.schema.json", reflect.TypeOf(model.AdviceCard{})},
	{"work_order.schema.json", reflect.TypeOf(model.WorkOrder{})},
	{"citation.schema.json", reflect.TypeOf(model.Citation{})},
	{"run_state.schema.json", reflect.TypeOf(model.RunState{})},
	{"approval_ticket.schema.json", reflect.TypeOf(model.ApprovalTicket{})},
}

var (
	rawMessageType = reflect.TypeOf(json.RawMessage{})
	timeType       = reflect.TypeOf(time.Time{})
)

// schemaSet 是解析后的全部 schema 文档，按 $id 索引，供 $ref 解析与编译共用。
type schemaSet struct {
	byFile map[string]map[string]any
	byID   map[string]map[string]any
}

func loadSchemas(t *testing.T) *schemaSet {
	t.Helper()
	set := &schemaSet{byFile: map[string]map[string]any{}, byID: map[string]map[string]any{}}
	for _, c := range contracts {
		b, err := os.ReadFile(filepath.Join(contractsDir, c.file))
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := json.Unmarshal(b, &doc); err != nil {
			t.Fatalf("%s: %v", c.file, err)
		}
		set.byFile[c.file] = doc
		set.byID[doc["$id"].(string)] = doc
	}
	return set
}

func (s *schemaSet) compile(t *testing.T, file string) *jsonschema.Schema {
	t.Helper()
	comp := jsonschema.NewCompiler()
	comp.AssertFormat()
	for id, doc := range s.byID {
		if err := comp.AddResource(id, doc); err != nil {
			t.Fatal(err)
		}
	}
	sch, err := comp.Compile(s.byFile[file]["$id"].(string))
	if err != nil {
		t.Fatalf("compile %s: %v", file, err)
	}
	return sch
}

// resolve 把 $ref（本文件 $defs 或其他契约 $id）展开为目标 schema。
func (s *schemaSet) resolve(root, node map[string]any) (map[string]any, map[string]any) {
	ref, ok := node["$ref"].(string)
	if !ok {
		return root, node
	}
	if name, local := strings.CutPrefix(ref, "#/$defs/"); local {
		return root, root["$defs"].(map[string]any)[name].(map[string]any)
	}
	target := s.byID[ref]
	return target, target
}

func TestContractSchemas_Version(t *testing.T) {
	set := loadSchemas(t)
	for _, c := range contracts {
		doc := set.byFile[c.file]
		if v, _ := doc["x-contract-version"].(float64); int(v) != model.ContractVersion {
			t.Errorf("%s: x-contract-version = %v, want %d", c.file, doc["x-contract-version"], model.ContractVersion)
		}
		wantID := "/contracts/v1/" + c.file
		if id, _ := doc["$id"].(string); !strings.HasSuffix(id, wantID) {
			t.Errorf("%s: $id = %q, want suffix %q", c.file, id, wantID)
		}
		set.compile(t, c.file) // schema 本身合法
	}
}

// TestContractSchemas_MatchStructs 双向比对：Go json tag 集合 = schema properties；
// 无 omitempty 的字段 = schema required；嵌套结构体递归比对。
func TestContractSchemas_MatchStructs(t *testing.T) {
	set := loadSchemas(t)
	for _, c := range contracts {
		t.Run(c.file, func(t *testing.T) {
			doc := set.byFile[c.file]
			set.compareStruct(t, c.typ.Name(), c.typ, doc, doc)
		})
	}
}

func (s *schemaSet) compareStruct(t *testing.T, path string, typ reflect.Type, root, node map[string]any) {
	t.Helper()
	props, _ := node["properties"].(map[string]any)
	required := map[string]bool{}
	if reqs, ok := node["required"].([]any); ok {
		for _, r := range reqs {
			required[r.(string)] = true
		}
	}
	goFields := map[string]bool{}
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		name, opts, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" || name == "-" {
			t.Errorf("%s.%s: missing json tag", path, f.Name)
			continue
		}
		goFields[name] = true
		prop, ok := props[name].(map[string]any)
		if !ok {
			t.Errorf("%s.%s: field not in schema properties", path, name)
			continue
		}
		optional := strings.Contains(opts, "omitempty")
		if optional == required[name] {
			t.Errorf("%s.%s: omitempty=%v but schema required=%v", path, name, optional, required[name])
		}
		if nested := structOf(f.Type); nested != nil {
			r, sub := s.resolve(root, prop)
			if items, isArr := sub["items"].(map[string]any); isArr {
				r, sub = s.resolve(r, items)
			}
			s.compareStruct(t, path+"."+name, nested, r, sub)
		}
	}
	for name := range props {
		if !goFields[name] {
			t.Errorf("%s.%s: schema property has no Go field", path, name)
		}
	}
	if node["additionalProperties"] != false {
		t.Errorf("%s: schema must set additionalProperties=false", path)
	}
}

// structOf 返回需要递归比对的结构体类型（剥掉指针与切片；time.Time、RawMessage 视为标量）。
func structOf(t reflect.Type) reflect.Type {
	if t == rawMessageType {
		return nil
	}
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || t == timeType {
		return nil
	}
	return t
}
