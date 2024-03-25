package main

import (
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/zhubiaook/selfup"
	"github.com/zhubiaook/selfup/fetcher"
)

var version = "v1"

func main() {
	slog.SetLogLoggerLevel(slog.LevelInfo)
	selfup.Run(selfup.Config{
		Program:          prog,
		MinFetchInterval: 2 * time.Second,
		Fetcher:          &fetcher.File{Path: "/tmp/demo/app"},
		TerminateTimeout: 5,
	})
}

func prog(state selfup.State) {
	slog.Debug("start program", "version", version)

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGUSR2, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)

	for {
		select {
		case <-quit:
			fmt.Println("quit")
			os.Exit(0)
		case <-time.After(2 * time.Second):
			fmt.Println("version:", version, " hello world")
		}
	}
}
