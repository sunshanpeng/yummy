package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/sunshanpeng/yummy/internal/controlplane/cli"
	"github.com/sunshanpeng/yummy/internal/controlplane/webui"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := cli.Execute(ctx, os.Args[1:], webui.FS()); err != nil {
		log.Fatal(err)
	}
}
