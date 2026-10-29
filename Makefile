GO      ?= go
SUITE   ?= regression
N       ?= 3
TAGS    ?=
BASELINE ?=

# 密钥只从 deploy/.env（不入库）或环境变量取；文件不存在时只读环境变量
-include deploy/.env
export

.PHONY: build test test-integration eval validate vet up minio-local migrate migrate-eval seed server reconcile

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

# L7 指标对账：生产库 agri_steward 某 UTC 日 MetricsService.Daily vs 直接 SQL（DAY=YYYY-MM-DD，缺省当天）
reconcile:
	$(GO) run ./cmd/reconcile $(if $(DAY),--day $(DAY))

# 一键回放评测集：默认 regression 套件、每样本 3 次；TAGS=L2 只回放某层样本。写 agri_eval.experiment / trial
# BASELINE=baseline_p0 时上传报告与快照到 report/{exp_id}/ 并写 eval/baselines/{BASELINE}.yaml（须 N ≥ 3）
eval:
	$(GO) run ./cmd/eval run --suite $(SUITE) --n $(N) $(if $(TAGS),--tags $(TAGS)) $(if $(BASELINE),--baseline $(BASELINE))

# 只做样本静态校验，不装配 agent
validate:
	$(GO) run ./cmd/eval validate
