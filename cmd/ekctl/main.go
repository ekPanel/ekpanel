// Command ekctl is the command-line tool for the ekPanel API. The work is in internal/cli.
package main

import (
	"context"
	"os"
	"os/signal"

	"ekpanel/internal/cli"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := cli.Run(ctx, cli.OSEnv(version), os.Args[1:])
	stop()
	os.Exit(code)
}
