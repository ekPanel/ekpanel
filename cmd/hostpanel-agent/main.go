// Command hostpanel-agent runs on every managed server and connects it to the panel.
//
//	hostpanel-agent enroll --panel https://panel.example.com --token <one-time token>
//	hostpanel-agent run
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"hostpanel/internal/agent"
)

var version = "dev"

const defaultConfig = "/etc/hostpanel/agent.json"

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
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
	case "version":
		fmt.Println(version)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		log.Error("hostpanel-agent", "err", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  hostpanel-agent enroll --panel URL --token TOKEN [--config PATH]
  hostpanel-agent run [--config PATH]
  hostpanel-agent version`)
}

func enroll(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("enroll", flag.ContinueOnError)
	panel := fs.String("panel", "", "panel URL, e.g. https://panel.example.com")
	token := fs.String("token", "", "one-time enrolment token from the panel")
	config := fs.String("config", defaultConfig, "where to save the agent credential")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *panel == "" || *token == "" {
		return fmt.Errorf("--panel and --token are required")
	}
	cfg, err := agent.Enroll(ctx, *panel, *token, *config)
	if err != nil {
		return err
	}
	fmt.Printf("Enrolled as server %s. Credential saved to %s.\nStart the agent with: hostpanel-agent run --config %s\n",
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
	return agent.Run(ctx, log, cfg, version)
}
