GO      ?= go
SUITE   ?= regression
N       ?= 3
TAGS    ?=

.PHONY: build test test-integration eval validate vet

build:
	$(GO) build ./...

vet:
	$(GO) vet ./...

test:
	$(GO) test ./...

# 涉及 MySQL / Redis / MinIO 的测试用 build tag integration，需先 docker compose up（L1 起才有）
test-integration:
	$(GO) test -tags integration ./...

# 一键回放评测集：默认 regression 套件、每样本 3 次；TAGS=L2 只回放某层样本
eval:
	$(GO) run ./cmd/eval run --suite $(SUITE) --n $(N) $(if $(TAGS),--tags $(TAGS))

# 只做样本静态校验，不装配 agent
validate:
	$(GO) run ./cmd/eval validate
