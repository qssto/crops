package model

import "testing"

// TestNotifyDedupKey（P1-L5 审查修复 10）：ListExpiring 的 NOT EXISTS 用 CONCAT(NotifyDedupPrefix, order_id, ':', ticket_id)
// 拼 dedup_key，须与写入侧 NotifyDedupKey 逐字节一致，否则反连接永远不命中、已提醒的票占满 LIMIT。
func TestNotifyDedupKey(t *testing.T) {
	tests := []struct {
		kind             NotifyKind
		order, ticket    uint64
		want, wantPrefix string
	}{
		{NotifyExpiringSoon, 9, 7, "notify:EXPIRING_SOON:9:7", "notify:EXPIRING_SOON:"},
		{NotifyPendingApproval, 10, 8, "notify:PENDING_APPROVAL:10:8", "notify:PENDING_APPROVAL:"},
		{NotifyDeviceOffline, 1, 0, "notify:DEVICE_OFFLINE:1:0", "notify:DEVICE_OFFLINE:"},
	}
	for _, tt := range tests {
		t.Run(string(tt.kind), func(t *testing.T) {
			if got := NotifyDedupKey(tt.kind, tt.order, tt.ticket); got != tt.want {
				t.Fatalf("NotifyDedupKey = %q, want %q", got, tt.want)
			}
			if got := NotifyDedupPrefix(tt.kind); got != tt.wantPrefix {
				t.Fatalf("NotifyDedupPrefix = %q, want %q", got, tt.wantPrefix)
			}
		})
	}
}
