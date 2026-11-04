GO      ?= go
SUITE   ?= regression
N       ?= 3
TAGS    ?=
BASELINE ?=

# 密钥只从 deploy/.env（不入库）或环境变量取；文件不存在时只读环境变量
-include deploy/.env
export

.PHONY: build test test-integration eval validate vet up minio-local migrate migrate-eval seed server worker reconcile devicesim

# P2 L7-a 评审修复：进 cmd/eval 命令行的变量先校验格式，再以单引号字面量传参（workflow 的 inputs 经环境变量进 make 参数）。
# shq 取变量原文（$(value)，不展开其中的 $(...)）并转成单引号 shell 字面量；ARG_VARS 不导出，免得 export 展开它们。
# chk_bad 在值匹配非法模式、chk_good 在值不匹配合法模式时以退出码 2 失败；报错只回显单引号字面量。
ARG_VARS := MODE SUITE N TAGS TOKEN_BUDGET BASELINE CANDIDATE REFERENCE EXP PROMPT_BASE
unexport $(ARG_VARS)
shq = '$(subst ','\'',$(value $(1)))'
arg_die = echo 'make: invalid $(1)='$(call shq,$(1)) >&2; exit 2
chk_bad = case $(call shq,$(1)) in $(2)) $(call arg_die,$(1));; esac
chk_good = case $(call shq,$(1)) in $(2)) ;; *) $(call arg_die,$(1));; esac
arg_lc := abcdefghijklmnopqrstuvwxyz0123456789
arg_uc := ABCDEFGHIJKLMNOPQRSTUVWXYZ
# 正整数（可空：TOKEN_BUDGET 空 = 不限）；必填项前加 ''|
ARG_INT_BAD := *[!0-9]*|0*
# baseline 名同 report.ValidateBaselineName：^[a-z0-9][a-z0-9._-]{0,63}$（可空）
ARG_NAME_BAD := [!$(arg_lc)]*|*[!$(arg_lc)._-]*|?????????????????????????????????????????????????????????????????*
# 套件：逗号分隔的小写名（如 regression,redteam），首字符须为字母数字
ARG_SUITE_BAD := ''|[!$(arg_lc)]*|*[!$(arg_lc)_,-]*
ARG_TAGS_BAD := *[!$(arg_lc)$(arg_uc)_,-]*
# git ref：不得以 - 开头（防选项注入），只许字母数字与 . _ / -
ARG_REF_BAD := ''|-*|*[!$(arg_lc)$(arg_uc)._/-]*

build:
	$(GO) build ./...

vet:
	$(GO) vet ./...

# 含 internal/archtest 分层守卫
test:
	$(GO) test ./...

# 真实连 docker MySQL / Redis（先 make up migrate-eval）；-p 1 避免各包同时建 trial schema
test-integration:
	$(GO) test -tags integration -count=1 -p 1 ./...

up:
	docker compose -f deploy/docker-compose.yml --env-file deploy/.env up -d

# docker 拉不到 minio 镜像时的替代：Homebrew minio 后台启动，端口 / 数据目录 / 凭据同 compose
minio-local:
	@mkdir -p deploy/data/minio
	MINIO_ROOT_USER="$$MINIO_USER" MINIO_ROOT_PASSWORD="$$MINIO_PASSWORD" nohup minio server deploy/data/minio \
		--address 127.0.0.1:9000 --console-address 127.0.0.1:9001 > deploy/data/minio.log 2>&1 &

# 业务库 agri_steward + MinIO bucket；MinIO 未启动时 MIGRATE_FLAGS=--skip-buckets
migrate:
	$(GO) run ./cmd/migrate up $(MIGRATE_FLAGS)

migrate-eval:
	$(GO) run ./cmd/migrate up --target eval

seed:
	$(GO) run ./cmd/migrate seed --file configs/seed/fields.yaml

server:
	$(GO) run ./cmd/server

# P1-L4-A 后台任务进程：outbox 轮询（下发 / 通知）、扫描任务、分区维护、开关订阅；可多副本
worker:
	$(GO) run ./cmd/worker

# P1-L2 本地设备模拟平台（只用于本地 / 预发演练；地址与 configs/devices.yaml 的 devicesim.base_url 一致）
devicesim:
	$(GO) run ./cmd/devicesim

# L7 指标对账：生产库 agri_steward 某 UTC 日 MetricsService.Daily vs 直接 SQL（DAY=YYYY-MM-DD，缺省当天）
reconcile:
	$(GO) run ./cmd/reconcile $(if $(DAY),--day $(DAY))

# 一键回放评测集：默认 regression 套件、每样本 3 次；TAGS=L2 只回放某层样本。写 agri_eval.experiment / trial
# BASELINE=baseline_p0 时上传报告与快照到 report/{exp_id}/ 并写 eval/baselines/{BASELINE}.yaml（须 N ≥ 3）
# P2 L7-a：MODE=smoke|nightly|release 时 suite / n / trigger 取预设（cmd/eval/mode.go），SUITE / N / TAGS 不再透传，
# 且必须给 TOKEN_BUDGET（CI 触发失败关闭）；不传 MODE 保持原行为，TOKEN_BUDGET 可选（空 = 不限）
TOKEN_BUDGET ?=
eval:
	@$(call chk_good,MODE,''|smoke|nightly|release); $(call chk_bad,SUITE,$(ARG_SUITE_BAD)); $(call chk_bad,N,''|$(ARG_INT_BAD)); $(call chk_bad,TAGS,$(ARG_TAGS_BAD)); $(call chk_bad,TOKEN_BUDGET,$(ARG_INT_BAD)); $(call chk_bad,BASELINE,$(ARG_NAME_BAD))
	$(GO) run ./cmd/eval run $(if $(value MODE),--mode $(call shq,MODE),--suite $(call shq,SUITE) --n $(call shq,N) $(if $(value TAGS),--tags $(call shq,TAGS))) $(if $(value TOKEN_BUDGET),--token-budget $(call shq,TOKEN_BUDGET)) $(if $(value BASELINE),--baseline $(call shq,BASELINE))

# 夜间全量：P2 L7-a 起改调 MODE=nightly（regression + redteam + capability、每样本 5 次、trigger nightly，须给 TOKEN_BUDGET）
.PHONY: eval-nightly
eval-nightly:
	$(MAKE) eval MODE=nightly TOKEN_BUDGET=$(call shq,TOKEN_BUDGET)

# P2 L7-a：补跑实验 EXP 中缺失与 INFRA_ERROR 的 trial（冻结 / 快照不符拒绝），补跑后重判作废；
# 含 holdout 套件的实验（如 release）须 HOLDOUT=1 且 AS_EVAL_READER 在读者名单中
.PHONY: eval-resume
eval-resume:
	@$(call chk_bad,EXP,''|$(ARG_INT_BAD))
	$(GO) run ./cmd/eval resume --exp $(call shq,EXP)$(if $(filter 1,$(HOLDOUT)), --holdout)

# P2 L7-a：prompts/ 相对 BASE（默认 origin/main）有改动时 configs/model.yaml 的 prompt_version 必须变（CI 调用）
PROMPT_BASE ?= origin/main
.PHONY: eval-promptcheck
eval-promptcheck:
	@$(call chk_bad,PROMPT_BASE,$(ARG_REF_BAD))
	$(GO) run ./cmd/eval promptcheck --base $(call shq,PROMPT_BASE)

# 只做样本静态校验，不装配 agent
validate:
	$(GO) run ./cmd/eval validate

# P2 L0：冻结基线（P1 实验加 BACKFILL=eval/baselines/xxx.yaml 补录快照）、对比（MODE=phase 为阶段级，REFERENCE 仅 phase）、A/A 阈值草稿
.PHONY: eval-freeze eval-compare eval-aa
eval-freeze:
	$(GO) run ./cmd/eval baseline freeze --name $(NAME) --exp $(EXP) $(if $(BACKFILL),--backfill $(BACKFILL))

eval-compare:
	@$(call chk_bad,BASELINE,''|$(ARG_NAME_BAD)); $(call chk_bad,CANDIDATE,''|$(ARG_INT_BAD)); $(call chk_good,MODE,''|factor|phase); $(call chk_bad,REFERENCE,$(ARG_INT_BAD))
	$(GO) run ./cmd/eval compare --baseline $(call shq,BASELINE) --candidate $(call shq,CANDIDATE) $(if $(value MODE),--mode $(call shq,MODE)) $(if $(value REFERENCE),--reference $(call shq,REFERENCE)) $(if $(FAIL_ON_UNRELEASABLE),--fail-on-unreleasable)

eval-aa:
	$(GO) run ./cmd/eval aa $(if $(FROM_EXPS),--from-exps $(FROM_EXPS),--runs $(or $(RUNS),3))

# P2 L2：人工队列入队、judge 校准（DIM=fact_consistency|conclusion_match|explanation_quality）、月度复核
.PHONY: eval-judge-queue eval-judge-calibrate eval-judge-recheck
eval-judge-queue:
	$(GO) run ./cmd/eval judge queue --exp $(EXP) $(if $(SEED),--seed $(SEED))

eval-judge-calibrate:
	$(GO) run ./cmd/eval judge calibrate --dim $(DIM) --exp $(EXP) $(if $(SEED),--seed $(SEED))

eval-judge-recheck:
	$(GO) run ./cmd/eval judge recheck --dim $(DIM) --exp $(EXP)
