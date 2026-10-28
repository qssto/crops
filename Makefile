GO      ?= go
SUITE   ?= regression
N       ?= 3
TAGS    ?=

# 密钥只从 deploy/.env（不入库）或环境变量取；文件不存在时只读环境变量
-include deploy/.env
export

.PHONY: build test test-integration eval validate vet up migrate migrate-eval seed server

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

# 业务库 agri_steward + MinIO bucket；MinIO 未启动时 MIGRATE_FLAGS=--skip-buckets
migrate:
	$(GO) run ./cmd/migrate up $(MIGRATE_FLAGS)

migrate-eval:
	$(GO) run ./cmd/migrate up --target eval

seed:
	$(GO) run ./cmd/migrate seed --file configs/seed/fields.yaml

server:
	$(GO) run ./cmd/server

# 一键回放评测集：默认 regression 套件、每样本 3 次；TAGS=L2 只回放某层样本。写 agri_eval.experiment / trial
eval:
	$(GO) run ./cmd/eval run --suite $(SUITE) --n $(N) $(if $(TAGS),--tags $(TAGS))

# 只做样本静态校验，不装配 agent
validate:
	$(GO) run ./cmd/eval validate
