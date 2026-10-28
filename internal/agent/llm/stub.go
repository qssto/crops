package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// Stub 是确定性桩 ChatModel：按关键词规则产出 classify / choose / explain 的 JSON 回复，
// 用于离线回归与开发环境（provider=stub 显式选择）。它不理解语义，只保证同一输入同一输出。
type Stub struct{}

var _ einomodel.BaseChatModel = Stub{}

// Generate 解析最后一条 user 消息里的 {"task","input"}，按任务分派。
func (Stub) Generate(_ context.Context, msgs []*schema.Message, _ ...einomodel.Option) (*schema.Message, error) {
	var user *schema.Message
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i] != nil && msgs[i].Role == schema.User {
			user = msgs[i]
			break
		}
	}
	if user == nil {
		return nil, fmt.Errorf("llm stub: no user message")
	}
	var env struct {
		Task  Task            `json:"task"`
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal([]byte(user.Content), &env); err != nil {
		return nil, fmt.Errorf("llm stub: decode task: %w", err)
	}
	out, err := stubReply(env.Task, env.Input)
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("llm stub: encode reply: %w", err)
	}
	return schema.AssistantMessage(string(b), nil), nil
}

// Stream 以单帧流返回 Generate 的结果。
func (s Stub) Stream(ctx context.Context, msgs []*schema.Message, opts ...einomodel.Option) (*schema.StreamReader[*schema.Message], error) {
	m, err := s.Generate(ctx, msgs, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{m}), nil
}

func stubReply(task Task, raw json.RawMessage) (any, error) {
	switch task {
	case TaskClassify:
		var in ClassifyInput
		if err := json.Unmarshal(raw, &in); err != nil {
			return nil, fmt.Errorf("llm stub: classify input: %w", err)
		}
		return StubClassify(in.Question), nil
	case TaskChoose:
		var in ChooseInput
		if err := json.Unmarshal(raw, &in); err != nil {
			return nil, fmt.Errorf("llm stub: choose input: %w", err)
		}
		return stubChoose(in), nil
	case TaskExplain:
		var in ExplainInput
		if err := json.Unmarshal(raw, &in); err != nil {
			return nil, fmt.Errorf("llm stub: explain input: %w", err)
		}
		return stubExplain(in), nil
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnknownTask, task)
	}
}

// 关键词规则表。顺序即优先级。
var (
	// 超出本农场水稻业务：其他作物、他人田块、外地、用户坚持越过规则层。
	outOfScope = regexp.MustCompile(`小麦|玉米|隔壁|老王|黑龙江|改种|就想用|就按\d`)
	// 业务内但知识未覆盖的主体（必须 KB_MISS，不得凭模型记忆回答）。
	unknownSubject = regexp.MustCompile(`冰雹|保险|理赔|富硒|补贴|海藻酸|稻虾|黑粉|多少钱|便宜|哪家店`)

	disasterWords   = regexp.MustCompile(`高温|热害|台风|倒伏|太旺|会不会倒|寒露风|降温|低温|淹|霜冻|35度|冰雹|暴雨|叶色卡\s*[4-9]`)
	protectStrong   = regexp.MustCompile(`用什么药|什么药|打药|除草|喷药|药剂`)
	irrigateWords   = regexp.MustCompile(`水层|灌水|晒田|排水|复水|断水|方水|灌多少|水怎么管|水位|水多深|灌到|补到|加到|没明水`)
	fertilizeWords  = regexp.MustCompile(`肥|缺|发黄|发白|不发|叶色|磷酸二氢钾|尿素`)
	protectWeak     = regexp.MustCompile(`病|虫|螟|飞虱|杂草|防治|喷雾器|药`)
	deficiencyWords = regexp.MustCompile(`缺|发黄|发白|不发|什么原因|褐色小斑点`)
	foliarWords     = regexp.MustCompile(`磷酸二氢钾|叶面`)
	dispatchWords   = regexp.MustCompile(`安排|派无人机|让无人机|无人机.*(去|起飞)|排个作业`)
	depthPattern    = regexp.MustCompile(`(?:灌到|补到|加到|复水到|复到)\s*(\d+(?:\.\d+)?)\s*厘米`)
	dilutionPattern = regexp.MustCompile(`稀释\s*(\d+(?:\.\d+)?)\s*倍`)
	waterPattern    = regexp.MustCompile(`(\d+(?:\.\d+)?)\s*升`)
	bagPattern      = regexp.MustCompile(`每袋\s*(\d+(?:\.\d+)?)\s*公斤`)
)

// targetWords 是防治对象关键词 → 登记表 target id，按顺序匹配。
var targetWords = []struct {
	re     *regexp.Regexp
	target string
}{
	{regexp.MustCompile(`飞虱`), "planthopper"},
	{regexp.MustCompile(`卷叶`), "leaf_roller"},
	{regexp.MustCompile(`纹枯`), "sheath_blight"},
	{regexp.MustCompile(`二化螟|枯鞘`), "stem_borer"},
	{regexp.MustCompile(`稻瘟|穗瘟|梭形`), "blast"},
	{regexp.MustCompile(`稻曲`), "false_smut"},
	{regexp.MustCompile(`除草|杂草`), "weeds"},
	{regexp.MustCompile(`恶苗`), "bakanae"},
}

// StubClassify 是桩分类规则（导出供测试与评测说明引用）。
func StubClassify(q string) ClassifyOutput {
	if outOfScope.MatchString(q) {
		return ClassifyOutput{InScope: false}
	}
	out := ClassifyOutput{InScope: true, Category: stubCategory(q)}
	out.Unknown = unknownSubject.MatchString(q)
	out.Slots = stubSlots(q)
	switch out.Category {
	case "protect":
		out.Target = stubTarget(q)
		out.Intent = protectIntent(q)
	case "fertilize":
		out.Intent = fertilizeIntent(q, out.Slots)
	case "irrigate":
		out.Intent = irrigateIntent(q)
	case "disaster":
		out.Intent = IntentRespond
	}
	out.Topic = stubTopic(out.Category, out.Intent)
	if out.Category == "disaster" {
		out.Topic = hazardTopics[out.Slots.Hazard]
	}
	return out
}

func stubCategory(q string) string {
	switch {
	case disasterWords.MatchString(q):
		return "disaster"
	case protectStrong.MatchString(q):
		return "protect"
	case irrigateWords.MatchString(q):
		return "irrigate"
	case fertilizeWords.MatchString(q):
		return "fertilize"
	default:
		return "protect" // 包括 protectWeak 与无法判断的主体（交给知识层 KB_MISS 兜底）
	}
}

func stubTarget(q string) string {
	for _, t := range targetWords {
		if t.re.MatchString(q) {
			return t.target
		}
	}
	return ""
}

// 意图关键词。
var (
	safetyWords     = regexp.MustCompile(`混|防护|几个小时`)
	timingWords     = regexp.MustCompile(`要不要|需要|该不该|到没到|能.*打吗|安排|派|打一[次遍]|排个作业|预防|什么时候`)
	identifyWords   = regexp.MustCompile(`是什么病|是不是|什么原因`)
	dosageWords     = regexp.MustCompile(`多少|兑|稀释|药量`)
	rateWords       = regexp.MustCompile(`多少|每亩|一共|整块田|补肥`)
	afterSprayWords = regexp.MustCompile(`打完.*药`)
	amountWords     = regexp.MustCompile(`多少方|灌多少`)
	drainWords      = regexp.MustCompile(`晒田|排水|断水|晒吗`)
	irrigateNow     = regexp.MustCompile(`要不要灌水|可以复水|灌水`)
)

func protectIntent(q string) string {
	switch {
	case safetyWords.MatchString(q):
		return IntentSafety
	case timingWords.MatchString(q):
		return IntentTiming
	case identifyWords.MatchString(q):
		return IntentIdentify
	case dosageWords.MatchString(q):
		return IntentDosage
	default:
		return IntentProduct
	}
}

func fertilizeIntent(q string, s Slots) string {
	switch {
	case deficiencyWords.MatchString(q):
		return IntentIdentify
	case s.Foliar:
		return IntentProduct
	case rateWords.MatchString(q) && !s.Dispatch:
		return IntentRate
	default:
		return IntentTiming
	}
}

func irrigateIntent(q string) string {
	switch {
	case afterSprayWords.MatchString(q):
		return IntentAfterSpray
	case amountWords.MatchString(q):
		return IntentAmount
	case drainWords.MatchString(q):
		return IntentDrain
	case irrigateNow.MatchString(q):
		return IntentIrrigateNow
	default:
		return IntentDepth
	}
}

func stubSlots(q string) Slots {
	s := Slots{Sprayer: SprayerKnapsack}
	if strings.Contains(q, "无人机") {
		s.Sprayer = SprayerUAV
	}
	switch {
	case strings.Contains(q, "基肥"):
		s.FertStage = "basal"
	case strings.Contains(q, "分蘖肥"):
		s.FertStage = "tillering"
	case strings.Contains(q, "保花肥"):
		s.FertStage = "panicle_protect"
	case strings.Contains(q, "促花肥"), strings.Contains(q, "穗肥"):
		s.FertStage = "panicle_promote"
	}
	switch {
	case strings.Contains(q, "农家肥"):
		s.FertProduct = ProductUnknown
	case strings.Contains(q, "复合肥"), strings.Contains(q, "15-15-15"):
		s.FertProduct = "compound"
	case strings.Contains(q, "尿素"):
		s.FertProduct = "urea"
	}
	s.Split = strings.Contains(q, "分两次")
	switch {
	case splitFirstWords.MatchString(q):
		s.SplitPart = SplitFirst
	case strings.Contains(q, "第二次"):
		s.SplitPart = SplitSecond
	}
	switch {
	case znWords.MatchString(q):
		s.Nutrient = NutrientZn
	case kWords.MatchString(q):
		s.Nutrient = NutrientK
	default:
		s.Nutrient = NutrientN
	}
	s.Hazard = stubHazard(q)
	s.Foliar = foliarWords.MatchString(q)
	s.Dispatch = dispatchWords.MatchString(q)
	s.TargetDepthCm = numberSlot(depthPattern, q)
	s.DilutionRatio = numberSlot(dilutionPattern, q)
	s.WaterL = numberSlot(waterPattern, q)
	s.BagKg = numberSlot(bagPattern, q)
	return s
}

// numberSlot 取 re 第一个分组里的数值；未匹配或无法解析时为 nil（槽位缺省）。
func numberSlot(re *regexp.Regexp, q string) *float64 {
	m := re.FindStringSubmatch(q)
	if m == nil {
		return nil
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return nil
	}
	return &v
}

// 槽位关键词：分次、缺素元素、灾害种类（按顺序匹配）。
var (
	splitFirstWords = regexp.MustCompile(`第一次|头一次`)
	znWords         = regexp.MustCompile(`发白|不发`)
	kWords          = regexp.MustCompile(`褐色小斑点|焦枯|叶尖`)
	hazardWords     = []struct {
		re     *regexp.Regexp
		hazard string
	}{
		{regexp.MustCompile(`高温|热害|35度`), HazardHeat},
		{regexp.MustCompile(`寒露风|降温|低温|霜冻`), HazardCold},
		{regexp.MustCompile(`台风`), HazardTyphoon},
		{regexp.MustCompile(`淹|暴雨`), HazardFlood},
		{regexp.MustCompile(`倒伏|太旺|会不会倒|叶色卡\s*[4-9]`), HazardLodging},
	}
	// hazardTopics 是灾害种类 → taxonomy 子类。
	hazardTopics = map[string]string{HazardHeat: "disaster.heat", HazardCold: "disaster.cold",
		HazardTyphoon: "disaster.storm", HazardFlood: "disaster.storm", HazardLodging: "disaster.lodging"}
)

func stubHazard(q string) string {
	for _, h := range hazardWords {
		if h.re.MatchString(q) {
			return h.hazard
		}
	}
	return ""
}

// stubTopic 把意图映射为 taxonomy 子类（可空：空表示只按一级类过滤）。
func stubTopic(category, intent string) string {
	topics := map[string]map[string]string{
		"protect":   {IntentIdentify: "protect.identify", IntentTiming: "protect.timing", IntentDosage: "protect.dosage", IntentProduct: "protect.product"},
		"fertilize": {IntentIdentify: "fertilize.deficiency", IntentRate: "fertilize.rate", IntentTiming: "fertilize.timing", IntentProduct: "fertilize.product"},
		"irrigate":  {IntentAmount: "irrigate.amount", IntentDrain: "irrigate.drain", IntentIrrigateNow: "irrigate.timing", IntentDepth: "irrigate.depth", IntentAfterSpray: "irrigate.depth"},
	}
	return topics[category][intent]
}

// stubChoose 选问题中点名的候选，否则选第一个。
func stubChoose(in ChooseInput) ChooseOutput {
	for _, c := range in.Candidates {
		if c.CommonName != "" && strings.Contains(in.Question, c.CommonName) {
			return ChooseOutput{ProductID: c.ID}
		}
	}
	if len(in.Candidates) == 0 {
		return ChooseOutput{}
	}
	return ChooseOutput{ProductID: in.Candidates[0].ID}
}

// stubExplain 用模板组织结论：规则层判定 + 依据 + 数值占位符。
func stubExplain(in ExplainInput) ExplainOutput {
	var b strings.Builder
	b.WriteString(in.Decision)
	if in.Product != "" {
		b.WriteString("；建议药剂：" + in.Product)
	}
	for i, n := range in.Numbers {
		if i == 0 {
			b.WriteString("；")
		} else {
			b.WriteString("，")
		}
		b.WriteString(n.Label + " " + n.Placeholder)
	}
	b.WriteString("（草拟，待专家确认）")
	return ExplainOutput{Conclusion: b.String(), Reasons: append([]string(nil), in.Reasons...)}
}
