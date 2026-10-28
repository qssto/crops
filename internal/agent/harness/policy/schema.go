package policy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// CompileSchema 编译工具声明里的 JSON Schema（断言 format），name 只用于报错定位。
// 返回的 Validator 接受 encoding/json（UseNumber）或 jsonschema.UnmarshalJSON 解码的值，并发安全。
func CompileSchema(name string, schema json.RawMessage) (Validator, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schema))
	if err != nil {
		return nil, fmt.Errorf("policy: schema %s is not JSON: %w", name, err)
	}
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	// 资源 URL 需转义 name（如 "#" 会被当作 fragment）。
	loc := "mem:///" + url.PathEscape(name)
	if err := c.AddResource(loc, doc); err != nil {
		return nil, fmt.Errorf("policy: schema %s: %w", name, err)
	}
	sch, err := c.Compile(loc)
	if err != nil {
		return nil, fmt.Errorf("policy: compile schema %s: %w", name, err)
	}
	return sch, nil
}
