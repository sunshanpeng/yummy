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
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := bootstrap.Run(ctx, "worker", os.Args[1:], nil); err != nil && !errors.Is(err, flag.ErrHelp) {
		log.Fatal(err)
	}
}
