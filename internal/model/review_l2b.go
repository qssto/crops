package model

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// P2-L2b 专家评审（评测库 agri_eval.review_task，P2 §6.3 SQL）：平台路由 /api/v1/admin/review-tasks，只限 kb_expert。
// 评测侧（internal/evalkit/store）与业务侧（internal/repository）共用本文件的取值与 ref_key 口径。

// review_task.task_type 取值。ONLINE_JUDGE 为 P2-L2b 新增：线上 online_judge_result 的 NEED_EXPERT 行入队，
// draft_ref 记 OnlineJudgeRef（复用列，不加迁移）。
const (
	ReviewCalibration    = "CALIBRATION"
	ReviewHumanQueue     = "HUMAN_QUEUE"
	ReviewMonthlyRecheck = "MONTHLY_RECHECK"
	ReviewBackflowExpect = "BACKFLOW_EXPECT"
	ReviewOnlineJudge    = "ONLINE_JUDGE"
)

// ReviewTaskTypes 是全部已登记任务类型（列表过滤校验用）。
var ReviewTaskTypes = []string{ReviewCalibration, ReviewHumanQueue, ReviewMonthlyRecheck, ReviewBackflowExpect, ReviewOnlineJudge}

// review_task.status 取值。
const (
	ReviewOpen     = "OPEN"
	ReviewAssigned = "ASSIGNED"
	ReviewDone     = "DONE"
	ReviewSkipped  = "SKIPPED"
)

// ReviewStatuses 是全部任务状态（列表过滤校验用）；ReviewPending 是「待评」（列表缺省）。
var (
	ReviewStatuses = []string{ReviewOpen, ReviewAssigned, ReviewDone, ReviewSkipped}
	ReviewPending  = []string{ReviewOpen, ReviewAssigned}
)

// 专家分数（1-3 有序量表，与 judge 一致）与主标 / 双标序号。
const (
	ExpertScoreMin = 1
	ExpertScoreMax = 3
	RaterPrimary   = 1
	RaterSecond    = 2
)

// 回流草稿的专家结论（BACKFLOW_EXPECT 提交体）。
const (
	BackflowDecisionMerge = "MERGE" // 补 expect，待并入回归集
	BackflowDecisionDrop  = "DROP"  // 不入库，须写原因
)

// 提交体上限。
const (
	MaxReviewReason      = 512      // DROP 原因（字符）
	MaxReviewExpectBytes = 16 << 10 // expect JSON 对象（字节）
	onlineJudgeRefPrefix = "ojr"    // OnlineJudgeRef 前缀
	onlineJudgeRefParts  = 4        // ojr:{tenant}:{farm}:{id}
	refKeySep            = "|"      // ref_key 拼接分隔符
	decimal              = 10       // strconv 进制
	uint64Bits           = 64       // strconv 位宽
)

// ErrReviewSubmit 表示提交体不合法（controller 回 400）。
var ErrReviewSubmit = errors.New("review submit invalid")

// ReviewTask 是一行 review_task。可空列用指针 / 空串。Holdout 不是列：读时按 trial.suite 判定
// （找不到 trial 也按 holdout 处理，失败关闭），为 true 时任何样本材料都不出接口。
type ReviewTask struct {
	ID           uint64
	TaskType     string
	QueueReason  string
	ExperimentID *uint64
	SampleID     string
	TrialIndex   *int
	DraftRef     string
	Dimension    string
	JudgeVersion string
	JudgeScore   *int // 只供评测侧对比；评审接口不回（盲评）
	RaterNo      int
	ExpertScore  *int
	AssigneeID   *uint64
	Status       string
	RefKey       string
	CreatedAt    time.Time
	DoneAt       *time.Time
	Holdout      bool
}

// ReviewRefKey 是去重键 ref_key = md5(task_type|experiment_id|sample_id|trial_index|dimension|draft_ref|rater_no)
// （NULL 记空串，rater_no 零值按 1）。评测侧 store.ReviewTask.Key 与业务侧入队共用。
func ReviewRefKey(taskType string, experimentID *uint64, sampleID string, trialIndex *int, dimension, draftRef string,
	raterNo int) string {
	exp, idx := "", ""
	if experimentID != nil {
		exp = strconv.FormatUint(*experimentID, decimal)
	}
	if trialIndex != nil {
		idx = strconv.Itoa(*trialIndex)
	}
	if raterNo == 0 {
		raterNo = RaterPrimary
	}
	raw := strings.Join([]string{taskType, exp, sampleID, idx, dimension, draftRef, strconv.Itoa(raterNo)}, refKeySep)
	sum := md5.Sum([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// Key 是本行的 ref_key。
func (t ReviewTask) Key() string {
	return ReviewRefKey(t.TaskType, t.ExperimentID, t.SampleID, t.TrialIndex, t.Dimension, t.DraftRef, t.RaterNo)
}

// OnlineJudgeRef 是 ONLINE_JUDGE 任务的 draft_ref：ojr:{tenant_id}:{farm_id}:{online_judge_result.id}。
// 回写业务库时按它取 scope，UPDATE 条件带 tenant_id 与 farm_id。
func OnlineJudgeRef(tenantID, farmID, id uint64) string {
	return strings.Join([]string{onlineJudgeRefPrefix, strconv.FormatUint(tenantID, decimal),
		strconv.FormatUint(farmID, decimal), strconv.FormatUint(id, decimal)}, ":")
}

// ParseOnlineJudgeRef 解析 OnlineJudgeRef；三段 ID 都须为正整数。
func ParseOnlineJudgeRef(ref string) (tenantID, farmID, id uint64, err error) {
	parts := strings.Split(ref, ":")
	if len(parts) != onlineJudgeRefParts || parts[0] != onlineJudgeRefPrefix {
		return 0, 0, 0, fmt.Errorf("online judge ref %q: want ojr:{tenant}:{farm}:{id}", ref)
	}
	var ids [3]uint64
	for i, p := range parts[1:] {
		v, perr := strconv.ParseUint(p, decimal, uint64Bits)
		if perr != nil || v == 0 {
			return 0, 0, 0, fmt.Errorf("online judge ref %q: part %d not a positive id", ref, i+1)
		}
		ids[i] = v
	}
	return ids[0], ids[1], ids[2], nil
}

// ReviewTaskFilter 是评审任务列表条件：TaskType 为空不过滤；Statuses 为空取 ReviewPending。
type ReviewTaskFilter struct {
	TaskType string
	Statuses []string
	Offset   int
	Limit    int
}

// ReviewSubmit 是专家提交体（contracts/review_submit_request.schema.json）：
// 打分类任务（CALIBRATION / HUMAN_QUEUE / MONTHLY_RECHECK / ONLINE_JUDGE）只带 score（1-3）；
// BACKFLOW_EXPECT 带 decision，MERGE 须带 expect（JSON 对象），DROP 须带 reason。
type ReviewSubmit struct {
	Score    *int            `json:"score,omitempty"`
	Decision string          `json:"decision,omitempty"`
	Expect   json.RawMessage `json:"expect,omitempty"`
	Reason   string          `json:"reason,omitempty"`
}

// ValidateReviewSubmit 按任务类型校验提交体；不合法返回 ErrReviewSubmit。
func ValidateReviewSubmit(taskType string, s ReviewSubmit) error {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: "+format, append([]any{ErrReviewSubmit}, args...)...)
	}
	switch taskType {
	case ReviewCalibration, ReviewHumanQueue, ReviewMonthlyRecheck, ReviewOnlineJudge:
		switch {
		case s.Score == nil:
			return bad("%s needs score", taskType)
		case *s.Score < ExpertScoreMin || *s.Score > ExpertScoreMax:
			return bad("score %d out of %d-%d", *s.Score, ExpertScoreMin, ExpertScoreMax)
		case s.Decision != "" || len(s.Expect) > 0 || s.Reason != "":
			return bad("%s takes only score", taskType)
		}
		return nil
	case ReviewBackflowExpect:
		return validateBackflowSubmit(s, bad)
	default:
		return bad("unknown task_type %q", taskType)
	}
}

func validateBackflowSubmit(s ReviewSubmit, bad func(string, ...any) error) error {
	if s.Score != nil {
		return bad("%s takes no score", ReviewBackflowExpect)
	}
	if utf8.RuneCountInString(s.Reason) > MaxReviewReason {
		return bad("reason over %d chars", MaxReviewReason)
	}
	switch s.Decision {
	case BackflowDecisionMerge:
		if len(s.Expect) > MaxReviewExpectBytes {
			return bad("expect over %d bytes", MaxReviewExpectBytes)
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(s.Expect, &obj); err != nil || len(obj) == 0 {
			return bad("MERGE needs a non-empty expect object")
		}
	case BackflowDecisionDrop:
		if strings.TrimSpace(s.Reason) == "" {
			return bad("DROP needs reason")
		}
		if len(s.Expect) > 0 {
			return bad("DROP takes no expect")
		}
	default:
		return bad("decision %q, want MERGE / DROP", s.Decision)
	}
	return nil
}

// ValidReviewTaskType 报告任务类型是否已登记。
func ValidReviewTaskType(t string) bool { return slices.Contains(ReviewTaskTypes, t) }

// ValidReviewStatus 报告任务状态是否已登记。
func ValidReviewStatus(s string) bool { return slices.Contains(ReviewStatuses, s) }

// 评审列表分页（GET /api/v1/admin/review-tasks?page=&page_size=）。
const (
	DefaultReviewPageSize = 20
	MaxReviewPageSize     = 100
)

// ReviewTaskView 是评审接口回给专家的任务（contracts/review_task_view.schema.json）。盲评：不含 judge_score；
// 专家分只回本人的。Holdout 为 true 时 Material 恒为空：只回 ID 与状态类字段，样本 ID / trial 定位都不出接口。
type ReviewTaskView struct {
	ID          uint64          `json:"id"`
	TaskType    string          `json:"task_type"`
	Status      string          `json:"status"`
	RaterNo     int             `json:"rater_no"`
	Dimension   string          `json:"dimension,omitempty"`
	Holdout     bool            `json:"holdout"`
	Mine        bool            `json:"mine"`
	ExpertScore *int            `json:"expert_score,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
	DoneAt      *time.Time      `json:"done_at,omitempty"`
	Material    *ReviewMaterial `json:"material,omitempty"`
}

// ReviewMaterial 是非 holdout 任务的定位信息（只有 ID / 版本 / 状态，不含任何样本文本）。
type ReviewMaterial struct {
	QueueReason  string             `json:"queue_reason,omitempty"`
	ExperimentID *uint64            `json:"experiment_id,omitempty"`
	SampleID     string             `json:"sample_id,omitempty"`
	TrialIndex   *int               `json:"trial_index,omitempty"`
	JudgeVersion string             `json:"judge_version,omitempty"`
	DraftRef     string             `json:"draft_ref,omitempty"`
	OnlineJudge  *OnlineJudgeTarget `json:"online_judge,omitempty"`
	Trial        *ReviewTrial       `json:"trial,omitempty"`
}

// OnlineJudgeTarget 是 ONLINE_JUDGE 任务要回写的业务库行（由 draft_ref 解析）。
type OnlineJudgeTarget struct {
	TenantID uint64 `json:"tenant_id"`
	FarmID   uint64 `json:"farm_id"`
	ResultID uint64 `json:"result_id"`
}

// ReviewTrial 是非 holdout 任务所挂 trial 的 suite / sample_version / status（详情接口才带）。
type ReviewTrial struct {
	Suite         string `json:"suite"`
	SampleVersion int    `json:"sample_version"`
	Status        string `json:"status"`
}

// View 是给专家 expertID 看的任务视图（不含 trial 摘要，由调用方按需挂到 Material.Trial）。
func (t ReviewTask) View(expertID uint64) ReviewTaskView {
	v := ReviewTaskView{ID: t.ID, TaskType: t.TaskType, Status: t.Status, RaterNo: t.RaterNo, Dimension: t.Dimension,
		Holdout: t.Holdout, Mine: t.AssigneeID != nil && *t.AssigneeID == expertID, CreatedAt: t.CreatedAt, DoneAt: t.DoneAt}
	if v.Mine {
		v.ExpertScore = t.ExpertScore
	}
	if t.Holdout {
		return v
	}
	m := &ReviewMaterial{QueueReason: t.QueueReason, ExperimentID: t.ExperimentID, SampleID: t.SampleID,
		TrialIndex: t.TrialIndex, JudgeVersion: t.JudgeVersion, DraftRef: t.DraftRef}
	if t.TaskType == ReviewOnlineJudge {
		if tn, fm, id, err := ParseOnlineJudgeRef(t.DraftRef); err == nil {
			m.OnlineJudge = &OnlineJudgeTarget{TenantID: tn, FarmID: fm, ResultID: id}
		}
	}
	v.Material = m
	return v
}

// ReviewTaskPage 是评审列表的一页。
type ReviewTaskPage struct {
	Items    []ReviewTaskView `json:"items"`
	Total    int64            `json:"total"`
	Page     int              `json:"page"`
	PageSize int              `json:"page_size"`
}

// ReviewSubmitResult 是提交结果：Replayed 为 true 表示同一专家同一内容的重复提交（未再产生副作用）。
type ReviewSubmitResult struct {
	Task     ReviewTaskView `json:"task"`
	Replayed bool           `json:"replayed"`
}
