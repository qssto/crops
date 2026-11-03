package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"agri-steward/internal/app"
	"agri-steward/internal/clock"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
	"agri-steward/internal/service"
)

// 灰度与关停子命令（P1 §6.9 技术实现第 1、4 条）：
//
//	auditctl gray-preflight --farm <id> [--tenant <id>] [--root .] [--redteam-max-age 36h]
//	    逐项打印 PASS / FAIL 与原因（D5、法规签字、告警规则、7 天内关停演练、redteam pass^5、agronomist、enable_advice），
//	    任一 FAIL 退出码 1。业务库与评测库都走只读连接；redteam 应有样本集读 --root 下 eval/suites/redteam/**。
//	auditctl kill-switch-report --since <RFC3339> [--expect-held N] [--drill]
//	    以 --since 之后第一条 GLOBAL 开→关 FLAG_CHANGE 审计时间为 T0、其后第一条 force_off:false 为计数上界，输出 §6.9 各计数，
//	    任一断言不成立退出码 1。--drill 须 --expect-held ≥ 1，额外经封链短事务写一条平台链 KILL_SWITCH_DRILL 审计
//	    （唯一的写，另开一条可写连接），断言不成立也写。

// errAssert 表示检查完成且有断言不成立（退出码 1，与运行失败区分）。
var errAssert = errors.New("assertion failed")

func cmdGrayPreflight(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("gray-preflight", flag.ContinueOnError)
	var c common
	c.register(fs)
	var o verifyOpts
	fs.Int64Var(&o.Farm, "farm", -1, "农场 id（必填，> 0）")
	fs.Int64Var(&o.Tenant, "tenant", -1, "租户 id（缺省按 farm_profile 解析；一个农场属多个租户时必填）")
	root := fs.String("root", ".", "仓库根目录（读 policy/、deploy/alerts/ 与 eval/suites/redteam/）")
	maxAge := fs.Duration("redteam-max-age", service.DefaultRedteamMaxAge, "redteam 实验 finished_at 时效")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if o.Farm <= 0 || o.Tenant < -1 || o.Tenant == 0 {
		return fmt.Errorf("%w: gray-preflight: --farm must be > 0 and --tenant (if given) > 0", errUsage)
	}
	if *maxAge <= 0 {
		return fmt.Errorf("%w: gray-preflight: --redteam-max-age must be > 0", errUsage)
	}
	cfg, err := c.load()
	if err != nil {
		return err
	}
	db, err := openReadOnly(ctx, cfg)
	if err != nil {
		return err
	}
	evalDSN, err := cfg.MySQL.ReadOnlyDSN(cfg.MySQL.EvalDatabase)
	if err != nil {
		return errors.Join(err, repository.Close(db))
	}
	evalDB, err := openDSN(ctx, evalDSN, cfg.Log.Level)
	if err != nil {
		return errors.Join(err, repository.Close(db))
	}
	runErr := func() error {
		roles := repository.UserRoleRepo{DB: db}
		k, err := resolveChain(ctx, roles, o)
		if err != nil {
			return err
		}
		g, err := service.NewGrayPreflight(service.GrayPreflightDeps{Files: os.DirFS(*root),
			Drills: repository.KillSwitchRepo{DB: db}, Redteam: repository.EvalRedteamRepo{DB: evalDB}, FarmRoles: roles,
			Flags: repository.FlagRepo{DB: db}, Clock: clock.System{}, RedteamMaxAge: *maxAge})
		if err != nil {
			return err
		}
		return runGrayPreflight(ctx, g, model.Scope{TenantID: k.TenantID, FarmID: k.FarmID}, stdout)
	}()
	return errors.Join(runErr, repository.Close(evalDB), repository.Close(db))
}

// preflightRunner 是 service.GrayPreflight（单测替身）。
type preflightRunner interface {
	Run(ctx context.Context, scope model.Scope) ([]service.CheckResult, error)
}

// runGrayPreflight 逐项打印结论；任一 FAIL 返回 errAssert。
func runGrayPreflight(ctx context.Context, g preflightRunner, scope model.Scope, out io.Writer) error {
	rs, err := g.Run(ctx, scope)
	if err != nil {
		return err
	}
	failed := 0
	for _, r := range rs {
		verdict := "PASS"
		if !r.Pass {
			verdict, failed = "FAIL", failed+1
		}
		fmt.Fprintf(out, "%s %s: %s\n", verdict, r.Name, r.Detail)
	}
	if failed > 0 || !service.AllPassed(rs) {
		fmt.Fprintf(out, "RESULT FAIL farm (%d,%d): %d/%d checks failed\n", scope.TenantID, scope.FarmID, failed, len(rs))
		return fmt.Errorf("%w: gray-preflight farm (%d,%d) %d checks failed", errAssert, scope.TenantID, scope.FarmID, failed)
	}
	fmt.Fprintf(out, "RESULT PASS farm (%d,%d): %d checks\n", scope.TenantID, scope.FarmID, len(rs))
	return nil
}

// killSwitchOpts 是 kill-switch-report 参数。
type killSwitchOpts struct {
	Since      time.Time
	ExpectHeld int64
	Drill      bool
}

// validate：--expect-held ≥ 0；--drill 须 ≥ 1（没有预置单的空演练 elapsed 恒为 0，不能当作演练通过）。
func (o killSwitchOpts) validate() error {
	switch {
	case o.ExpectHeld < 0:
		return fmt.Errorf("%w: kill-switch-report: --expect-held must be >= 0", errUsage)
	case o.Drill && o.ExpectHeld < 1:
		return fmt.Errorf("%w: kill-switch-report: --drill needs --expect-held >= 1 (preset orders)", errUsage)
	}
	return nil
}

func cmdKillSwitchReport(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("kill-switch-report", flag.ContinueOnError)
	var c common
	c.register(fs)
	since := fs.String("since", "", "RFC3339 时间（必填），只用于定位其后第一条 GLOBAL force_off=true 的 FLAG_CHANGE 审计")
	var o killSwitchOpts
	fs.Int64Var(&o.ExpectHeld, "expect-held", 0, "演练预置单中未发出的张数（断言 DISPATCH_HELD 之和不小于它；0 不断言，--drill 时须 ≥ 1）")
	fs.BoolVar(&o.Drill, "drill", false, "演练：额外写一条 KILL_SWITCH_DRILL 审计（平台链）")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	t, err := time.Parse(time.RFC3339Nano, *since)
	if err != nil {
		return fmt.Errorf("%w: kill-switch-report: --since must be RFC3339: %w", errUsage, err)
	}
	if err := o.validate(); err != nil {
		return err
	}
	o.Since = t.UTC()
	cfg, err := c.load()
	if err != nil {
		return err
	}
	db, err := openReadOnly(ctx, cfg)
	if err != nil {
		return err
	}
	deps := service.KillSwitchDeps{Store: repository.KillSwitchRepo{DB: db}, Flags: repository.FlagRepo{DB: db},
		Clock: clock.System{}}
	var writer *repository.DB
	if o.Drill { // 只有演练审计这一条写：单独开可写连接，报告查询仍走只读会话
		logger, err := app.NewLogger(os.Stderr, cfg.Log.Level)
		if err != nil {
			return errors.Join(err, repository.Close(db))
		}
		if writer, err = app.OpenMySQL(ctx, cfg, cfg.MySQL.Database, clock.System{}, logger); err != nil {
			return errors.Join(err, repository.Close(db))
		}
		deps.Audit = repository.AuditRepo{DB: writer}
	}
	runErr := func() error {
		k, err := service.NewKillSwitchReporter(deps)
		if err != nil {
			return err
		}
		return runKillSwitchReport(ctx, k, o, stdout)
	}()
	errs := []error{runErr, repository.Close(db)}
	if writer != nil {
		errs = append(errs, repository.Close(writer))
	}
	return errors.Join(errs...)
}

// killSwitchRunner 是 service.KillSwitchReporter（单测替身）。
type killSwitchRunner interface {
	Report(ctx context.Context, since time.Time, expectHeld int64) (service.KillSwitchReport, error)
	RecordDrill(ctx context.Context, rep service.KillSwitchReport) error
}

// runKillSwitchReport 打印报告；--drill 时写演练审计；断言不成立返回 errAssert。
func runKillSwitchReport(ctx context.Context, k killSwitchRunner, o killSwitchOpts, out io.Writer) error {
	rep, err := k.Report(ctx, o.Since, o.ExpectHeld)
	if err != nil {
		return err
	}
	printKillSwitchReport(out, rep)
	if o.Drill {
		if err := k.RecordDrill(ctx, rep); err != nil {
			return err
		}
		fmt.Fprintln(out, "drill audit: KILL_SWITCH_DRILL written (platform chain)")
	}
	if !rep.Passed() {
		fmt.Fprintf(out, "RESULT FAIL: %d assertions failed\n", len(rep.Failures))
		return fmt.Errorf("%w: kill-switch-report %d assertions failed", errAssert, len(rep.Failures))
	}
	fmt.Fprintln(out, "RESULT PASS")
	return nil
}

// printKillSwitchReport 按 §6.9 技术实现第 4 条输出各计数与灰度档位。
func printKillSwitchReport(out io.Writer, r service.KillSwitchReport) {
	fmt.Fprintf(out, "T0 %s (FLAG_CHANGE audit %d)\n", r.T0.Format(time.RFC3339Nano), r.FlagChangeID)
	if r.ReleasedAt != nil {
		fmt.Fprintf(out, "released %s (FLAG_CHANGE audit %d), counts bounded to [T0, released)\n",
			r.ReleasedAt.Format(time.RFC3339Nano), r.ReleaseID)
	} else {
		fmt.Fprintln(out, "not released yet, counts from T0 to now")
	}
	maxSent := "none"
	if r.MaxSentAt != nil {
		maxSent = r.MaxSentAt.Format(time.RFC3339Nano)
	}
	fmt.Fprintf(out, "max(sent_at WHERE mode='REAL') %s, minus T0 = %dms (limit %dms)\n", maxSent, r.ElapsedMS,
		service.KillSwitchMaxElapsed.Milliseconds())
	fmt.Fprintf(out, "REAL attempts sent after T0: %d\n", r.RealSentAfter)
	fmt.Fprintf(out, "REAL attempts sent after T0 with flag_read_at >= T0: %d (want 0)\n", r.FlagReadAfter)
	fmt.Fprintf(out, "DISPATCH_HELD after T0 across %d gray farms: %d (expect >= %d)\n", len(r.Farms), r.HeldTotal,
		r.ExpectHeld)
	for _, f := range r.Farms {
		fmt.Fprintf(out, "  farm (%d,%d) enable_robot_dispatch=%s held=%d\n", f.TenantID, f.FarmID, f.Value, f.Held)
	}
	for _, f := range r.Failures {
		fmt.Fprintf(out, "FAIL %s\n", f)
	}
}
