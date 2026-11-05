package quality

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"agri-steward/internal/clock"
	"agri-steward/internal/model"
)

// Notifier 推送日报摘要（钉钉；仓库里还没有钉钉通道，生产先用 RecordingNotifier，待运行）。
type Notifier interface {
	Notify(ctx context.Context, title, text string) error
}

// RecordingNotifier 是记录型 stub：写日志并留在内存（测试断言用），不对外发送。
type RecordingNotifier struct {
	Logger *slog.Logger
	mu     sync.Mutex
	sent   []string
}

// Notify 实现 Notifier。
func (n *RecordingNotifier) Notify(ctx context.Context, title, text string) error {
	n.mu.Lock()
	n.sent = append(n.sent, title+"\n"+text)
	n.mu.Unlock()
	if n.Logger != nil {
		n.Logger.InfoContext(ctx, "quality daily notify (recording stub)", "title", title)
	}
	return nil
}

// Sent 返回已记录的消息。
func (n *RecordingNotifier) Sent() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.sent...)
}

// Summary 是日报推送摘要：每个指标一行「离线 / 线上」，末行为日报对象键（只含聚合值）。
func Summary(rep Report, key string) (title, text string) {
	var b strings.Builder
	for _, r := range rep.Rows {
		fmt.Fprintf(&b, "%s：离线 %s %s｜线上 %s %s\n", r.Title, r.Offline.Text, r.Offline.Diff, r.Online.Text, r.Online.Diff)
	}
	b.WriteString(key)
	return "可信度日报 " + rep.Date, b.String()
}

// ---- 专家回写（待专家：暂无入口，流程用合成专家分跑通） ----

// ErrBadExpertScore 表示专家分不在 1..3。
var ErrBadExpertScore = errors.New("quality: expert score must be 1..3")

// Reviewer 是专家巡检回写。
type Reviewer struct {
	Store ReviewStore
	Clock clock.Clock
}

// Review 用乐观锁回写专家分；row_ver 已变返回 repository.ErrJudgeRowConflict（调用方重读后再提交）。
func (r Reviewer) Review(ctx context.Context, scope model.Scope, id uint64, rowVer, score int, expertID uint64) error {
	if score < 1 || score > judgeTopScore {
		return fmt.Errorf("%w: got %d", ErrBadExpertScore, score)
	}
	if expertID == 0 {
		return errors.New("quality: expert id required")
	}
	return r.Store.ReviewJudgeResult(ctx, scope, id, rowVer, score, expertID, r.Clock.Now())
}
