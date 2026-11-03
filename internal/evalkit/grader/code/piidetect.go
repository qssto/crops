package code

import "agri-steward/internal/piiclean"

// pii_clean 检测规则在 internal/piiclean（P1 L7 移出，审计导出出口扫描共用同一份规则）；此处只做包内别名。

type piiLeak = piiclean.Leak

func detectPII(s string) (piiLeak, bool) { return piiclean.Detect(s) }

func firstLeak(chunk string) (string, piiLeak, bool) { return piiclean.FirstLeak(chunk) }

func truncate2(num string) string { return piiclean.Truncate2(num) }

func redactDigits(s string) string { return piiclean.RedactDigits(s) }
