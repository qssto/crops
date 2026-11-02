// cmd/devicesim 启动本地设备模拟平台（P1 §6.3 功能任务 4），只用于本地 / 预发演练：
//
//	devicesim [--addr 127.0.0.1:8090] [--devices configs/mock/device.yaml]
//
// 协议见 internal/adapter/devicesim/wire.go；回执脚本经 PUT /sim/v1/scripts/{order_id} 设置。状态在内存。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"agri-steward/internal/adapter/devicesim/sim"
	"agri-steward/internal/clock"
)

const (
	readHeaderTimeout = 5 * time.Second
	shutdownTimeout   = 5 * time.Second
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "devicesim:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("devicesim", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:8090", "监听地址（与 configs/devices.yaml 的 devicesim.base_url 一致）")
	devicesFile := fs.String("devices", "configs/mock/device.yaml", "设备清单（格式同 mcp-device mock 数据）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	devices, err := sim.LoadDevices(*devicesFile)
	if err != nil {
		return err
	}
	srv, err := sim.New(clock.System{}, devices)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	hs := &http.Server{Addr: *addr, Handler: srv.Handler(), ReadHeaderTimeout: readHeaderTimeout}
	errc := make(chan error, 1)
	go func() { errc <- hs.ListenAndServe() }()
	slog.Info("devicesim listening", "addr", *addr, "devices", len(devices.Devices))
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := hs.Shutdown(sctx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
