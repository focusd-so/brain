package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/focusd-so/brain/cmd/serve"
	"github.com/joho/godotenv"
	"github.com/urfave/cli/v3"
)

func main() {
	_ = godotenv.Load()

	root := &cli.Command{Name: "focusd", Commands: []*cli.Command{
		serve.Command,
	}}

	if err := root.Run(context.Background(), os.Args); err != nil {
		slog.Error("failed to run command", "error", err)
		os.Exit(1)
	}
}
