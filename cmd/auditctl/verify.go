package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"

	"agri-steward/internal/app"
	"agri-steward/internal/config"
	"agri-steward/internal/repository"
	"agri-steward/internal/service"
)

// verifyOpts 是 verify 子命令参数；Tenant < 0 表示未指定（按 user_role 解析农场归属租户）。
type verifyOpts struct {
	Farm, Tenant   int64
	FromSeq, ToSeq uint64
	Manifest       string
}

// tenantLookup 按农场找归属租户（repository.UserRoleRepo 实现）。
type tenantLookup interface {
	FarmTenants(ctx context.Context, farmID uint64) ([]uint64, error)
}

func cmdVerify(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	var c common
	c.register(fs)
	var o verifyOpts
	fs.Int64Var(&o.Farm, "farm", -1, "农场 id（必填；0 表示平台链）")
	fs.Int64Var(&o.Tenant, "tenant", -1, "租户 id（缺省按 user_role 解析；一个农场属多个租户时必填）")
	fs.Uint64Var(&o.FromSeq, "from-seq", 0, "起始 chain_seq（缺省 1）")
	fs.Uint64Var(&o.ToSeq, "to-seq", 0, "结束 chain_seq（缺省 head）")
	fs.StringVar(&o.Manifest, "from-manifest", "", "导出包 manifest.json（未启用）")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if o.Manifest != "" {
		return errNotEnabled
	}
	if err := o.validate(); err != nil {
		return err
	}
	cfg, err := c.load()
	if err != nil {
		return err
	}
	db, err := openReadOnly(ctx, cfg)
	if err != nil {
		return err
	}
	runErr := runVerify(ctx, repository.AuditChainRepo{DB: db}, repository.UserRoleRepo{DB: db}, o, stdout)
	return errors.Join(runErr, repository.Close(db))
}

func (o verifyOpts) validate() error {
	switch {
	case o.Farm < 0:
		return fmt.Errorf("%w: verify: --farm is required", errUsage)
	case o.Tenant < -1:
		return fmt.Errorf("%w: verify: --tenant must be >= 0", errUsage)
	case o.ToSeq != 0 && o.FromSeq > o.ToSeq:
		return fmt.Errorf("%w: verify: --from-seq %d > --to-seq %d", errUsage, o.FromSeq, o.ToSeq)
	}
	return nil
}

// runVerify 解析链键并校验；有断点返回 errBroken（输出第一个断点）。
func runVerify(ctx context.Context, r service.ChainReader, tenants tenantLookup, o verifyOpts, out io.Writer) error {
	k, err := resolveChain(ctx, tenants, o)
	if err != nil {
		return err
	}
	head, err := r.Head(ctx, k)
	if err != nil {
		return fmt.Errorf("verify chain (%d,%d) head: %w", k.TenantID, k.FarmID, err)
	}
	if o.FromSeq > head.ChainSeq { // 起点越过 head：区间为空，按用法错误（exit 2）而非「OK rows=0」
		return fmt.Errorf("%w: verify: --from-seq %d > head chain_seq %d of chain (%d,%d)", errUsage, o.FromSeq,
			head.ChainSeq, k.TenantID, k.FarmID)
	}
	res, err := service.VerifyChain(ctx, r, k, o.FromSeq, o.ToSeq, nil)
	if err != nil {
		return fmt.Errorf("verify chain (%d,%d): %w", k.TenantID, k.FarmID, err)
	}
	if res.Break != nil {
		fmt.Fprintf(out, "BROKEN chain (%d,%d) range [%d,%d]: %s\n", k.TenantID, k.FarmID, res.From, res.To, res.Break)
		return fmt.Errorf("%w: chain (%d,%d) %s", errBroken, k.TenantID, k.FarmID, res.Break.Kind)
	}
	fmt.Fprintf(out, "OK chain (%d,%d) range [%d,%d] rows=%d\n", k.TenantID, k.FarmID, res.From, res.To, res.Rows)
	return nil
}

// resolveChain 由参数得到链键：--tenant 优先；农场 0 缺省为平台链 (0,0)；否则按 user_role 唯一归属租户。
func resolveChain(ctx context.Context, tenants tenantLookup, o verifyOpts) (repository.ChainKey, error) {
	farm := uint64(o.Farm)
	if o.Tenant >= 0 {
		return repository.ChainKey{TenantID: uint64(o.Tenant), FarmID: farm}, nil
	}
	if farm == 0 {
		return repository.ChainKey{}, nil
	}
	ts, err := tenants.FarmTenants(ctx, farm)
	if err != nil {
		return repository.ChainKey{}, fmt.Errorf("resolve tenant of farm %d: %w", farm, err)
	}
	switch len(ts) {
	case 0:
		return repository.ChainKey{}, fmt.Errorf("farm %d has no tenant binding; pass --tenant", farm)
	case 1:
		return repository.ChainKey{TenantID: ts[0], FarmID: farm}, nil
	}
	return repository.ChainKey{}, fmt.Errorf(multiTenant, farm, ts)
}

// anchorDatePattern 限定 --date 格式（拼进对象 key）。
var anchorDatePattern = regexp.MustCompile(`^[0-9]{8}$`)

func cmdAnchorCheck(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("anchor-check", flag.ContinueOnError)
	var c common
	c.register(fs)
	date := fs.String("date", "", "锚点日期 yyyymmdd（必填）")
	file := fs.String("anchor-file", "", "本地锚点文件（缺省从锚点 bucket 读 anchor/{date}.json）")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if !anchorDatePattern.MatchString(*date) {
		return fmt.Errorf("%w: anchor-check: --date must be yyyymmdd, got %q", errUsage, *date)
	}
	cfg, err := c.load()
	if err != nil {
		return err
	}
	var body []byte
	versions := 1
	if *file != "" {
		body, err = os.ReadFile(*file)
	} else {
		var obj repository.AnchorObject
		obj, err = fetchAnchor(ctx, cfg, *date)
		body, versions = obj.Body, obj.Versions
	}
	if err != nil {
		return err
	}
	db, err := openReadOnly(ctx, cfg)
	if err != nil {
		return err
	}
	runErr := runAnchorCheck(ctx, repository.AuditChainRepo{DB: db}, *date, body, versions, stdout)
	return errors.Join(runErr, repository.Close(db))
}

// fetchAnchor 从锚点 bucket 读 anchor/{date}.json 的最早版本（需 MinIO 密钥，只从环境变量取）。
func fetchAnchor(ctx context.Context, cfg *config.Config, date string) (repository.AnchorObject, error) {
	mc, err := app.NewMinIO(cfg)
	if err != nil {
		return repository.AnchorObject{}, err
	}
	return repository.MinIOAnchorStore{Client: mc, Bucket: cfg.Audit.AnchorBucket}.GetAnchor(ctx, service.AnchorKey(date))
}

// runAnchorCheck 校验锚点文件整体 sha256 并逐链比对；锚点对象有多个版本（被重写）或任何不一致返回 errBroken。
func runAnchorCheck(ctx context.Context, r service.ChainReader, date string, body []byte, versions int, out io.Writer) error {
	if versions > 1 {
		fmt.Fprintf(out, "BROKEN anchor %s has %d versions (rewritten); checking earliest\n", date, versions)
	}
	a, err := service.ParseAnchor(body)
	if errors.Is(err, service.ErrAnchorDigest) {
		fmt.Fprintf(out, "BROKEN anchor %s: %v\n", date, err)
		return fmt.Errorf("%w: anchor %s digest", errBroken, date)
	}
	if err != nil {
		return err
	}
	if a.Date != date {
		fmt.Fprintf(out, "BROKEN anchor file date %q != --date %s\n", a.Date, date)
		return fmt.Errorf("%w: anchor date mismatch", errBroken)
	}
	ms, err := service.CheckAnchor(ctx, r, a)
	if err != nil {
		return fmt.Errorf("check anchor %s: %w", date, err)
	}
	for _, m := range ms {
		fmt.Fprintf(out, "BROKEN %s\n", m)
	}
	if len(ms) > 0 {
		return fmt.Errorf("%w: anchor %s: %d chain(s) mismatched", errBroken, date, len(ms))
	}
	if versions > 1 {
		return fmt.Errorf("%w: anchor %s rewritten (%d versions)", errBroken, date, versions)
	}
	fmt.Fprintf(out, "OK anchor %s chains=%d sha256=%s\n", date, len(a.Chains), a.SHA256)
	return nil
}
