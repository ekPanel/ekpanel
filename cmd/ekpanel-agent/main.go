// Command ekpanel-agent runs on every managed server and connects it to the panel.
//
//	ekpanel-agent enroll --panel panel.example.com:7443 --token <one-time token> --ca-pin sha256:…
//	ekpanel-agent run
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"ekpanel/internal/agent"
)

var version = "dev"

const defaultConfig = "/etc/ekpanel/agent.json"

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	slog.SetDefault(log)
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "enroll":
		err = enroll(ctx, os.Args[2:])
	case "run":
		err = run(ctx, log, os.Args[2:])
	case "files":
		// Internal: one file manager action, started by the agent itself (see agent.RunFileWorker).
		agent.RunFileWorker(os.Stdin, os.Stdout)
	case "version":
		fmt.Println(version)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		log.Error("ekpanel-agent", "err", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  ekpanel-agent enroll --panel HOST[:7443] --token TOKEN --ca-pin sha256:... [--config PATH]
  ekpanel-agent run [--config PATH]
  ekpanel-agent version`)
}

func enroll(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("enroll", flag.ContinueOnError)
	panel := fs.String("panel", "", "panel address for agents, e.g. panel.example.com:7443")
	token := fs.String("token", "", "one-time enrolment token from the panel")
	pin := fs.String("ca-pin", "", "fingerprint of the panel's agent CA, shown with the token (sha256:...)")
	config := fs.String("config", defaultConfig, "where to save the agent credential")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *panel == "" || *token == "" || *pin == "" {
		return fmt.Errorf("--panel, --token and --ca-pin are required; copy the full command from the panel")
	}
	cfg, err := agent.Enroll(ctx, *panel, *token, *pin, *config)
	if err != nil {
		return err
	}
	fmt.Printf("Enrolled as server %s. Credential saved to %s.\nStart the agent with: ekpanel-agent run --config %s\n",
		cfg.ServerID, *config, *config)
	return nil
}

func run(ctx context.Context, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	config := fs.String("config", defaultConfig, "agent credential file written by enroll")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := agent.LoadConfig(*config)
	if err != nil {
		return err
	}
	return agent.Run(ctx, log, cfg, *config, version)
}
