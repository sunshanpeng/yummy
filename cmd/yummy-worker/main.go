package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/sunshanpeng/yummy/internal/transport/workercli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := workercli.Execute(ctx, os.Args[1:]); err != nil {
		log.Fatal(err)
	}
}
