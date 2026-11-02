package service

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"agri-steward/internal/model"
)

// P1 L5 通知文案（configs/texts/notify.zh-CN.yaml）：每种 NotifyKind 一条模板，缺任一 key 启动失败。

// NotifyTextsVersion 是文案文件的 version 字段。
const NotifyTextsVersion = 1

// ErrBadNotifyTexts 表示文案文件不合法（缺 key、未知 key、空模板或未知字段）。
var ErrBadNotifyTexts = errors.New("service: bad notify texts")

// NotifyKinds 是处理器认识的全部通知类型；文案文件须逐一给出。
var NotifyKinds = []model.NotifyKind{model.NotifyPendingApproval, model.NotifyExpiringSoon,
	model.NotifyRecheckReturned, model.NotifyDeviceOffline}

// NotifyTexts 是 kind → 文案模板。模板只允许占位 {order_id} / {ticket_id}（不放人名、手机号等 PII）。
type NotifyTexts struct {
	byKind map[model.NotifyKind]string
}

type notifyTextFile struct {
	Version int                         `yaml:"version"`
	Texts   map[model.NotifyKind]string `yaml:"texts"`
}

// LoadNotifyTexts 读取并校验文案文件。
func LoadNotifyTexts(path string) (*NotifyTexts, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("service: read notify texts: %w", err)
	}
	t, err := ParseNotifyTexts(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return t, nil
}

// ParseNotifyTexts 解析并校验：未知字段、version 不符、缺任一 NotifyKinds、出现未知 kind、模板为空均报 ErrBadNotifyTexts。
func ParseNotifyTexts(b []byte) (*NotifyTexts, error) {
	var f notifyTextFile
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("%w: decode: %w", ErrBadNotifyTexts, err)
	}
	if f.Version != NotifyTextsVersion {
		return nil, fmt.Errorf("%w: version %d, want %d", ErrBadNotifyTexts, f.Version, NotifyTextsVersion)
	}
	for kind := range f.Texts {
		if !slices.Contains(NotifyKinds, kind) {
			return nil, fmt.Errorf("%w: unknown kind %q", ErrBadNotifyTexts, kind)
		}
	}
	for _, kind := range NotifyKinds {
		if strings.TrimSpace(f.Texts[kind]) == "" {
			return nil, fmt.Errorf("%w: missing text for %s", ErrBadNotifyTexts, kind)
		}
	}
	return &NotifyTexts{byKind: f.Texts}, nil
}

// NotifyTextKey 是审计 / 日志里记录的文案 key（不记正文）。
func NotifyTextKey(kind model.NotifyKind) string { return "notify." + string(kind) }

// Render 返回 kind 的文案（替换 {order_id} / {ticket_id}）；kind 未知返回 false。
func (t *NotifyTexts) Render(kind model.NotifyKind, orderID, ticketID uint64) (string, bool) {
	tpl, ok := t.byKind[kind]
	if !ok {
		return "", false
	}
	return strings.NewReplacer("{order_id}", strconv.FormatUint(orderID, 10),
		"{ticket_id}", strconv.FormatUint(ticketID, 10)).Replace(tpl), true
}
