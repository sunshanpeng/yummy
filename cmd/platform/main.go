package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"example.com/multitopology-demo/internal/bootstrap"
	"example.com/multitopology-demo/internal/webui"
)

func main() {
	mode, args, err := parseMode(os.Args[1:])
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := bootstrap.Run(ctx, mode, args, webui.FS()); err != nil && !errors.Is(err, flag.ErrHelp) {
		log.Fatal(err)
	}
}
