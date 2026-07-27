package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/sunshanpeng/yummy/internal/bootstrap"
	"github.com/sunshanpeng/yummy/internal/webui"
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
