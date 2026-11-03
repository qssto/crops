package constraint

import (
	"bytes"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// RegulationSignoff 是 policy/regulation_map.yaml 头部的法务签字元数据（P1-L8-B）：由法务签字后填写，工程侧不代填。
// gray-preflight 要求三项都非空；启动加载（ParseRegulationMap）只校验字段名，不要求已签字。
type RegulationSignoff struct {
	LegalSigner   string `yaml:"legal_signer"`    // 签字人
	LegalSignedAt string `yaml:"legal_signed_at"` // 签字日期
	RecordRef     string `yaml:"record_ref"`      // 签字记录位置（纸质 / 系统编号）
}

// Missing 返回未填写的字段名（yaml 键）；空切片表示已签字。
func (s RegulationSignoff) Missing() []string {
	var out []string
	for _, f := range []struct{ key, val string }{
		{"legal_signer", s.LegalSigner}, {"legal_signed_at", s.LegalSignedAt}, {"record_ref", s.RecordRef},
	} {
		if strings.TrimSpace(f.val) == "" {
			out = append(out, f.key)
		}
	}
	return out
}

// ParseRegulationSignoff 只解析头部 signoff：KnownFields 严格解码 signoff（与启动 ParseRegulationMap 一致，
// 拼错的签字字段报错而不是被当成未签）；其余顶层键由 inline map 接住，条目不校验，不依赖约束注册表。
func ParseRegulationSignoff(b []byte) (RegulationSignoff, error) {
	var f struct {
		Signoff *RegulationSignoff `yaml:"signoff"`
		Rest    map[string]any     `yaml:",inline"`
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return RegulationSignoff{}, fmt.Errorf("%w: decode signoff: %w", ErrBadRegulationMap, err)
	}
	if f.Signoff == nil {
		return RegulationSignoff{}, fmt.Errorf("%w: header signoff missing", ErrBadRegulationMap)
	}
	return *f.Signoff, nil
}
