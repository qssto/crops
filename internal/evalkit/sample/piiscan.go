package sample

import (
	"bytes"
	"fmt"
	"strings"

	"agri-steward/internal/piiclean"
)

// RulePIIClean 是样本文件 pii_clean 扫描规则名（P2 L1-T）。
const RulePIIClean = "pii_clean"

// piiPlaceholder 替换声明过的合成敏感值后再扫描。
const piiPlaceholder = "[PII]"

// checkPIIClean 对样本文件原文逐行跑 pii_clean 检测（internal/piiclean，与评测打分器、审计导出同一份独立规则），
// 命中即违规：样本会进仓库、报告与 transcript，不得夹带真实敏感信息。
// 例外只有声明过的合成值：expect.pii（脱敏断言用）与 synthetic_pii（kind 表达不了的，如银行卡号），
// 二者都须原样出现在 question 中；扫描前把声明值替换为占位符，同一行里其余未声明的敏感信息照样报出。
// 声明值本身须被 piiclean 判为敏感信息才豁免（否则声明 "1" 就能把行内所有 1 换掉、拆散真号码），
// 且只替换两侧不是数字的完整出现（声明的手机号不会吃掉更长号码里的一段）。
// Raw 为空（非 LoadDir 加载）时只校验声明。
func checkPIIClean(s *Sample) []string {
	var msgs []string
	var declared []string
	for _, p := range s.Expect.PII {
		if v := strings.TrimSpace(p.Value); v != "" && isPII(v) {
			declared = append(declared, v)
		}
	}
	for i, v := range s.SyntheticPII {
		switch v = strings.TrimSpace(v); {
		case v == "" || !strings.Contains(s.Question, v):
			msgs = append(msgs, fmt.Sprintf("synthetic_pii[%d] %q must be non-empty and appear in question", i, v))
		case !isPII(v):
			msgs = append(msgs, fmt.Sprintf("synthetic_pii[%d] %q is not detected as PII, nothing to exempt", i, v))
		default:
			declared = append(declared, v)
		}
	}
	for i, line := range bytes.Split(s.Raw, []byte("\n")) {
		if l, ok := piiclean.Detect(maskDeclared(string(line), declared)); ok {
			msgs = append(msgs, fmt.Sprintf("line %d: undeclared %s %s (declare synthetic values in expect.pii / synthetic_pii, or remove them)",
				i+1, l.Kind, l.Value))
		}
	}
	return msgs
}

// isPII：piiclean 在声明值中检出敏感信息。
func isPII(v string) bool {
	_, ok := piiclean.Detect(v)
	return ok
}

// maskDeclared 把 line 中每个声明值两侧不是数字的完整出现替换为占位符。
func maskDeclared(line string, declared []string) string {
	for _, v := range declared {
		var b strings.Builder
		rest := line
		for {
			i := strings.Index(rest, v)
			if i < 0 {
				b.WriteString(rest)
				break
			}
			end := i + len(v)
			if isDigitBefore(rest, i) || isDigitAt(rest, end) {
				b.WriteString(rest[:i+1])
				rest = rest[i+1:]
				continue
			}
			b.WriteString(rest[:i])
			b.WriteString(piiPlaceholder)
			rest = rest[end:]
		}
		line = b.String()
	}
	return line
}

// isDigitBefore / isDigitAt 判断 s 中位置 i 前一个 / 位置 i 处的字节是否为 ASCII 数字。
func isDigitBefore(s string, i int) bool { return i > 0 && s[i-1] >= '0' && s[i-1] <= '9' }

func isDigitAt(s string, i int) bool { return i < len(s) && s[i] >= '0' && s[i] <= '9' }
