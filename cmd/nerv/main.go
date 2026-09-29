package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/mvaldes14/nerv/internal/api"
	"github.com/mvaldes14/nerv/internal/config"
	"github.com/mvaldes14/nerv/internal/investigation"
	"github.com/mvaldes14/nerv/internal/llm"
)

func main() {
	if err := run(os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "nerv: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) < 2 {
		usage()
		return errors.New("missing command")
	}

	cfg := config.Load()
	svc := investigation.NewService(investigation.Dependencies{
		LLM: llm.NewEchoProvider(cfg.LLMProvider, cfg.LLMModel),
	})

	switch args[1] {
	case "ask":
		return runAsk(args[2:], svc)
	case "serve":
		return runServe(args[2:], cfg, svc)
	case "version":
		fmt.Println("nerv dev")
		return nil
	case "help", "-h", "--help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", args[1])
	}
}

func runAsk(args []string, svc *investigation.Service) error {
	fs := flag.NewFlagSet("ask", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	timeout := fs.Duration("timeout", 30*time.Second, "investigation timeout")
	if err := fs.Parse(args); err != nil {
		return err
	}
	question := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if question == "" {
		return errors.New("ask requires a question")
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	finding, err := svc.Ask(ctx, investigation.Request{Question: question, Source: "cli"})
	if err != nil {
		return err
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(finding)
}

func runServe(args []string, cfg config.Config, svc *investigation.Service) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	addr := fs.String("addr", cfg.Addr, "listen address")
	if err := fs.Parse(args); err != nil {
		return err
	}

	handler := api.NewHandler(api.Dependencies{
		Service:   svc,
		AuthToken: cfg.AuthToken,
		Logger:    slog.New(slog.NewTextHandler(os.Stderr, nil)),
	})

	srv := &http.Server{
		Addr:              *addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	fmt.Fprintf(os.Stderr, "nerv listening on %s\n", *addr)
	return srv.ListenAndServe()
}

func usage() {
	fmt.Fprintf(os.Stderr, `Usage:
  nerv ask [--timeout 30s] <question>
  nerv serve [--addr 127.0.0.1:8080]
  nerv version
`)
}
