package service

import (
	"bufio"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// MetricRegistry 是进程内指标注册表，按 Prometheus 文本格式 0.0.4 暴露（GET /metrics）。
// 偏离说明（P1 L7-B）：brief 批准的 prometheus/client_golang 依赖下载被权限拒绝，这里用标准库实现最小子集
// （counter / gauge / histogram，有限 label）；接入 client_golang 后只需替换本文件与 MetricsObserver 的构造。
// 每个 App 一个实例（非全局），测试互不干扰。
type MetricRegistry struct {
	mu       sync.Mutex
	families []*metricFamily
	byName   map[string]*metricFamily
}

// MetricKind 是指标类型。
type MetricKind string

const (
	MetricCounter   MetricKind = "counter"
	MetricGauge     MetricKind = "gauge"
	MetricHistogram MetricKind = "histogram"
)

// MetricInfo 是一个指标族的声明（规则文件校验与 label 白名单测试用）。
type MetricInfo struct {
	Name   string
	Kind   MetricKind
	Labels []string
}

type metricFamily struct {
	info    MetricInfo
	help    string
	buckets []float64
	mu      sync.Mutex
	series  map[string]*metricSeries
}

type metricSeries struct {
	values []string
	value  float64
	counts []uint64 // histogram：各桶非累计计数
	sum    float64
	count  uint64
}

// NewMetricRegistry 构造空注册表。
func NewMetricRegistry() *MetricRegistry {
	return &MetricRegistry{byName: map[string]*metricFamily{}}
}

func (r *MetricRegistry) register(kind MetricKind, name, help string, buckets []float64, labels ...string) *metricFamily {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.byName[name]; dup {
		panic("service: metric registered twice: " + name) // 只在构造期调用，属编程错误
	}
	f := &metricFamily{info: MetricInfo{Name: name, Kind: kind, Labels: labels}, help: help, buckets: buckets,
		series: map[string]*metricSeries{}}
	r.families = append(r.families, f)
	r.byName[name] = f
	return f
}

// Families 返回已注册的指标族（按注册顺序）。
func (r *MetricRegistry) Families() []MetricInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]MetricInfo, 0, len(r.families))
	for _, f := range r.families {
		out = append(out, MetricInfo{Name: f.info.Name, Kind: f.info.Kind, Labels: slices.Clone(f.info.Labels)})
	}
	return out
}

// get 取 label 值对应的序列，不存在时创建；调用方持 f.mu。label 个数不符属编程错误，按空值补齐 / 截断。
func (f *metricFamily) get(values []string) *metricSeries {
	vs := make([]string, len(f.info.Labels))
	copy(vs, values)
	key := strings.Join(vs, "\xff")
	s, ok := f.series[key]
	if !ok {
		s = &metricSeries{values: vs}
		if f.info.Kind == MetricHistogram {
			s.counts = make([]uint64, len(f.buckets))
		}
		f.series[key] = s
	}
	return s
}

// add 给序列加 v。counter 只增：负值与 NaN 属调用方编程错误，丢弃不计（Observer 接口不返回错误，单测覆盖）。
func (f *metricFamily) add(v float64, values ...string) {
	if f.info.Kind == MetricCounter && !(v >= 0) {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.get(values).value += v
}

// touch 预置一条 0 值序列，已存在则不动（新序列首样本问题，见 MetricsObserver.Preseed）。
func (f *metricFamily) touch(values ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.get(values)
}

func (f *metricFamily) observe(v float64, values ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.get(values)
	s.sum += v
	s.count++
	// 首个上界 >= v 的桶（le 含等号）；大于全部上界只进 +Inf
	if i, _ := slices.BinarySearch(f.buckets, v); i < len(f.buckets) {
		s.counts[i]++
	}
}

// replace 用一组 (label 值 → 数值) 整体替换 gauge 的全部序列。
func (f *metricFamily) replace(points map[string]float64, label func(string) []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.series = make(map[string]*metricSeries, len(points))
	for k, v := range points {
		f.get(label(k)).value = v
	}
}

// WriteText 按 Prometheus 文本格式写出全部指标（族按注册顺序，序列按 label 值排序，输出稳定）。
// 每个族只在锁内复制快照，格式化与写网络都在锁外，慢客户端不阻塞埋点。
func (r *MetricRegistry) WriteText(w io.Writer) error {
	r.mu.Lock()
	families := slices.Clone(r.families)
	r.mu.Unlock()
	bw := bufio.NewWriter(w)
	for _, f := range families {
		writeFamily(bw, f.info, f.help, f.buckets, f.snapshot())
	}
	return bw.Flush()
}

// snapshot 在族锁内按 label 值排序复制全部序列。
func (f *metricFamily) snapshot() []metricSeries {
	f.mu.Lock()
	defer f.mu.Unlock()
	keys := make([]string, 0, len(f.series))
	for k := range f.series {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	out := make([]metricSeries, 0, len(keys))
	for _, k := range keys {
		s := f.series[k]
		out = append(out, metricSeries{values: s.values, value: s.value, counts: slices.Clone(s.counts), sum: s.sum,
			count: s.count})
	}
	return out
}

// writeFamily 格式化一个族的快照（不持锁）；series.values 构造后不再修改，可与埋点共享。
func writeFamily(w *bufio.Writer, info MetricInfo, help string, buckets []float64, series []metricSeries) {
	name := info.Name
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", name, escapeHelp(help), name, info.Kind)
	for _, s := range series {
		if info.Kind != MetricHistogram {
			fmt.Fprintf(w, "%s%s %s\n", name, labelText(info.Labels, s.values, ""), formatFloat(s.value))
			continue
		}
		var cum uint64
		for i, b := range buckets {
			cum += s.counts[i]
			fmt.Fprintf(w, "%s_bucket%s %d\n", name, labelText(info.Labels, s.values, formatFloat(b)), cum)
		}
		fmt.Fprintf(w, "%s_bucket%s %d\n", name, labelText(info.Labels, s.values, "+Inf"), s.count)
		fmt.Fprintf(w, "%s_sum%s %s\n", name, labelText(info.Labels, s.values, ""), formatFloat(s.sum))
		fmt.Fprintf(w, "%s_count%s %d\n", name, labelText(info.Labels, s.values, ""), s.count)
	}
}

func labelText(names, values []string, le string) string {
	if len(names) == 0 && le == "" {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, n := range names {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(n + `="` + escapeLabel(values[i]) + `"`)
	}
	if le != "" {
		if len(names) > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`le="` + le + `"`)
	}
	b.WriteByte('}')
	return b.String()
}

var (
	labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	helpEscaper  = strings.NewReplacer(`\`, `\\`, "\n", `\n`)
)

func escapeLabel(s string) string { return labelEscaper.Replace(s) }
func escapeHelp(s string) string  { return helpEscaper.Replace(s) }

func formatFloat(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case math.IsNaN(v):
		return "NaN"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// metricsContentType 是 Prometheus 文本格式 0.0.4 的 Content-Type。
const metricsContentType = "text/plain; version=0.0.4; charset=utf-8"

// Handler 返回 GET /metrics 处理器（不经业务鉴权；只监听 config metrics 段配置的地址）。写出失败（连接断开等）只记 Warn。
func (r *MetricRegistry) Handler(log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", metricsContentType)
		if err := r.WriteText(w); err != nil && log != nil {
			log.WarnContext(req.Context(), "metrics: write response", "err", err)
		}
	})
}
