# eval/signoffs：NEED_SIGNOFF 签字文件

`cmd/eval gate` 判定为 NEED_SIGNOFF（成本 / P95 / 压缩触发率劣化超阈值，或 judge 维度未评）时，读 `eval/signoffs/{experiment_id}.md`。信任根是 `--trust-ref`（缺省 `origin/main`），不是被测分支：

- 文件已进信任 ref 的树，且与工作区内容逐字节一致；当前 HEAD 历史里改过该文件的提交都是信任 ref 的祖先（分支上改写再改回也不算）；
- `.github/CODEOWNERS`、`eval/gates/owners.yaml` 取信任 ref 的版本（被测分支上的改动不生效）；
- 信任 ref 历史里该文件的每个提交作者邮箱（`git log -z --format=%H%x00%ae%x00%G?`）非空、在 owners.yaml 中，且其 handle 是该路径的 CODEOWNERS owner；owners.yaml `require_signed_commits: true` 时每个提交 `%G?` 须为 `G`（缺省 false）；
- 首个非空行 `experiment_id: {id}`，其后写签字理由。

本地与 CI 同一口径（CI 须 `fetch-depth: 0` 才有 `origin/main` 全历史）。release 门禁只在 `refs/heads/main` 上跑，其他 ref 的 release 明确失败。

通过后写 `experiment.signed_by`（owners.yaml 的 user_id），gate 退出 0；否则退出非 0。签字只解除 NEED_SIGNOFF，不能覆盖 BLOCK_* / NOT_ATTRIBUTABLE。

提交作者邮箱可伪造。待部署方：main 分支保护 + 要求签名提交（GitHub「Require signed commits」，并把 owners.yaml `require_signed_commits` 置 true，CI 需导入 owner 公钥使 `%G?` 能得到 G）。

当前 CODEOWNERS / owners.yaml 均为占位账号（待真实账号），签字在替换前恒不通过。
