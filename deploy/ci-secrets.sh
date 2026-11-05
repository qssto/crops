#!/usr/bin/env bash
# 把评测门禁（.github/workflows/eval-gate.yml）要的 GitHub secrets 写进仓库：
#   EVAL_MYSQL_ROOT_PASSWORD / EVAL_REDIS_PASSWORD / EVAL_MINIO_USER / EVAL_MINIO_PASSWORD / EVAL_AUTH_JWT_SECRET
#     取自 deploy/.env 同名键（去掉 EVAL_ 前缀）；CI 起的是临时容器，可以另给一组值（导出同名环境变量覆盖）。
#   EVAL_MIGRATIONS_TGZ  = base64(tar czf migrations/)，只打包 migrations/ 下的 .sql（迁移不入库，CI 从这里还原）。
#   EVAL_HOLDOUT_TOKEN   只在导出了 EVAL_HOLDOUT_TOKEN 环境变量时写（能读 holdout 私有仓的 fine-grained token）。
# 依赖 gh（已 gh auth login，对仓库有 admin 权限）。值经 stdin 交给 gh，不出现在命令行与输出里。
# 用法：deploy/ci-secrets.sh [--dry-run]   （--dry-run 只列出将写入的 secret 名与大小）
set -euo pipefail

cd "$(dirname "$0")/.."
dry_run=false
[[ "${1:-}" == "--dry-run" ]] && dry_run=true

env_file=deploy/.env
[[ -f "$env_file" ]] || { echo "缺少 $env_file" >&2; exit 1; }
if ! $dry_run; then
  command -v gh >/dev/null || { echo "需要 gh（brew install gh && gh auth login）" >&2; exit 1; }
fi

# 读 deploy/.env 的一个键（环境变量优先）；不 source 整个文件，避免执行其中的内容。
env_value() {
  local key=$1
  if [[ -n "${!key:-}" ]]; then printf '%s' "${!key}"; return; fi
  local line
  line=$(grep -E "^${key}=" "$env_file" | tail -n 1 || true)
  [[ -n "$line" ]] || { echo "deploy/.env 缺少 $key" >&2; return 1; }
  line=${line#*=}
  line=${line%\"}; line=${line#\"}
  printf '%s' "$line"
}

put_secret() {
  local name=$1 value=$2
  [[ -n "$value" ]] || { echo "$name 为空，拒绝写入" >&2; exit 1; }
  if $dry_run; then
    echo "would set $name (${#value} bytes)"
  else
    printf '%s' "$value" | gh secret set "$name" --body -
    echo "set $name"
  fi
}

for key in MYSQL_ROOT_PASSWORD REDIS_PASSWORD MINIO_USER MINIO_PASSWORD AUTH_JWT_SECRET; do
  put_secret "EVAL_${key}" "$(env_value "$key")"
done

# 只收 .sql，路径固定以 migrations/ 开头（eval-gate 解包时同样校验）。
sql_files=()
while IFS= read -r f; do sql_files+=("$f"); done < <(find migrations -type f -name '*.sql' | LC_ALL=C sort)
(( ${#sql_files[@]} > 0 )) || { echo "migrations/ 下没有 .sql" >&2; exit 1; }
tgz=$(tar czf - "${sql_files[@]}" | base64 | tr -d '\n')
# GitHub secret 上限 48 KB
(( ${#tgz} < 48000 )) || { echo "迁移包 base64 后 ${#tgz} 字节，超过 secret 上限 48KB" >&2; exit 1; }
put_secret EVAL_MIGRATIONS_TGZ "$tgz"

if [[ -n "${EVAL_HOLDOUT_TOKEN:-}" ]]; then
  put_secret EVAL_HOLDOUT_TOKEN "$EVAL_HOLDOUT_TOKEN"
else
  echo "未导出 EVAL_HOLDOUT_TOKEN，跳过（release 模式需要它读 holdout 私有仓）"
fi
