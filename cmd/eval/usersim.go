package main

import (
	"flag"
	"path/filepath"

	"agri-steward/internal/evalkit/usersim"
)

// usersimFlags 是 run / resume 的模拟用户通道配置（P2 L7-b，§9.2）：user_sim 样本按 persona / instruction 多轮续问，
// 模型独立配置（eval.usersim.model / model_tier，现只有录制回放，待真实模型），缺录制的 trial 记 infra_error。
// 非 user_sim 样本不经通道，单轮回放与快照不变。
type usersimFlags struct {
	config     string
	recordings string
}

func (f *usersimFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&f.config, "usersim-config", "", "模拟用户模型配置（eval.usersim.model / model_tier）；空 = --repo 下 "+usersim.DefaultConfig)
	fs.StringVar(&f.recordings, "usersim-recordings", "", "模拟用户录制目录（{sample_id}.yaml，按 (turn, 对话哈希) 取回复）；空 = --repo 下 "+usersim.DefaultRecordingsDir)
}

// attach 加载模拟用户通道挂到 ls（newRunner 注入 Runner）；空路径取 repo 下默认位置；配置或录制非法时失败关闭。
func (f *usersimFlags) attach(ls *loadedSamples, repo string) error {
	config, recordings := f.config, f.recordings
	if config == "" {
		config = filepath.Join(repo, usersim.DefaultConfig)
	}
	if recordings == "" {
		recordings = filepath.Join(repo, usersim.DefaultRecordingsDir)
	}
	sim, cfg, err := usersim.Load(config, recordings)
	if err != nil {
		return err
	}
	ls.users, ls.usersimCfg = sim, cfg
	return nil
}
