package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	_ "time/tzdata" // ADR-0003：内嵌时区数据库，支持极简镜像下的 TZ。

	"weread-cron/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(cli.Run(ctx, os.Args[1:], os.Environ(), os.Stdout, os.Stderr))
}
