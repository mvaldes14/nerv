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
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/mvaldes14/nerv/internal/api"
	"github.com/mvaldes14/nerv/internal/config"
	"github.com/mvaldes14/nerv/internal/decision"
	"github.com/mvaldes14/nerv/internal/investigation"
	k8stools "github.com/mvaldes14/nerv/internal/kubernetes"
	"github.com/mvaldes14/nerv/internal/llm"
	"github.com/mvaldes14/nerv/internal/notify"
	"github.com/mvaldes14/nerv/internal/store"
	"github.com/mvaldes14/nerv/internal/telemetry"
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
	if args[1] == "version" {
		fmt.Println("nerv dev")
		return nil
	}
	if args[1] == "help" || args[1] == "-h" || args[1] == "--help" {
		usage()
		return nil
	}

	cfg := config.Load()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	shutdownTelemetry, err := telemetry.Setup(ctx, "nerv", cfg.OTLPEndpoint, cfg.OTLPInsecure)
	if err != nil {
		return err
	}
	defer shutdownTelemetry(context.Background())

	svc, cleanup, err := buildService(ctx, cfg, logger)
	if err != nil {
		return err
	}
	defer cleanup()

	switch args[1] {
	case "ask":
		return runAsk(args[2:], svc)
	case "serve":
		return runServe(ctx, args[2:], cfg, svc, logger)
	default:
		usage()
		return fmt.Errorf("unknown command %q", args[1])
	}
}

func buildService(ctx context.Context, cfg config.Config, logger *slog.Logger) (*investigation.Service, func(), error) {
	postgres, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { postgres.Close() }

	dbosCtx, err := dbos.NewContext(ctx, dbos.Config{DatabaseURL: cfg.DatabaseURL, AppName: "nerv"})
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("create DBOS context: %w", err)
	}

	var provider investigation.LLM
	if cfg.LLMProvider == "echo" {
		provider = llm.NewEchoProvider("echo", cfg.LLMModel)
	} else {
		provider = llm.NewOpenAICompatible(cfg.AgentgatewayURL, cfg.AgentgatewayToken, cfg.LLMProvider, cfg.LLMModel, cfg.HTTPTimeout)
	}

	var collector investigation.Collector
	kubeClient, err := k8stools.New(cfg.Kubeconfig)
	if err != nil {
		logger.Warn("Kubernetes tools disabled", "error", err)
	} else {
		collector = kubeClient
	}

	var decisionEngine investigation.DecisionEngine
	if cfg.DecisionGatewayURL != "" {
		decisionEngine = decision.NewClient(cfg.DecisionGatewayURL, cfg.DecisionGatewayToken, cfg.DecisionName, cfg.DecisionVersion, cfg.HTTPTimeout)
	}

	var notifier investigation.Notifier
	if cfg.GotifyURL != "" || cfg.GotifyToken != "" {
		notifier = notify.NewGotify(cfg.GotifyURL, cfg.GotifyToken, cfg.HTTPTimeout, postgres)
	}

	svc, err := investigation.NewService(investigation.Dependencies{
		DBOS:      dbosCtx,
		LLM:       provider,
		Collector: collector,
		Decision:  decisionEngine,
		Store:     postgres,
		Notifier:  notifier,
	})
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	if err := dbos.Launch(dbosCtx); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("launch DBOS: %w", err)
	}
	return svc, func() {
		dbos.Shutdown(dbosCtx, 10*time.Second)
		postgres.Close()
	}, nil
}

func runAsk(args []string, svc *investigation.Service) error {
	fs := flag.NewFlagSet("ask", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	timeout := fs.Duration("timeout", 60*time.Second, "investigation timeout")
	namespace := fs.String("namespace", "", "Kubernetes namespace")
	kind := fs.String("kind", "", "Kubernetes resource kind")
	name := fs.String("name", "", "Kubernetes resource name")
	container := fs.String("container", "", "pod container")
	if err := fs.Parse(args); err != nil {
		return err
	}
	question := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if question == "" {
		return errors.New("ask requires a question")
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	req := investigation.Request{Question: question, Source: "cli"}
	if *name != "" {
		req.Target = &investigation.Target{Namespace: *namespace, Kind: *kind, Name: *name, Container: *container}
	}
	finding, err := svc.Ask(ctx, req)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(finding)
}

func runServe(ctx context.Context, args []string, cfg config.Config, svc *investigation.Service, logger *slog.Logger) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	addr := fs.String("addr", cfg.Addr, "listen address")
	if err := fs.Parse(args); err != nil {
		return err
	}

	handler := api.NewHandler(api.Dependencies{Service: svc, AuthToken: cfg.AuthToken, Logger: logger})
	srv := &http.Server{Addr: *addr, Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	errCh := make(chan error, 1)
	go func() {
		logger.Info("NERV listening", "addr", *addr)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `Usage:
  nerv ask [--timeout 60s] [--namespace ns --kind pod --name pod] <question>
  nerv serve [--addr 127.0.0.1:8080]
  nerv version
`)
}
