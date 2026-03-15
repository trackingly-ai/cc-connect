package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:9820", "listen address")
	project := flag.String("project", "echo", "project name to register")
	flag.Parse()

	dataDir, err := os.MkdirTemp("", "cc-connect-mcpfixture-*")
	if err != nil {
		log.Fatalf("mkdir temp: %v", err)
	}
	defer os.RemoveAll(dataDir)

	jobMgr, err := core.NewJobManager(dataDir)
	if err != nil {
		log.Fatalf("job manager: %v", err)
	}

	engine := core.NewEngine(*project, &fixtureAgent{}, nil, "", core.LangEnglish)
	jobMgr.RegisterRunner(*project, engine.JobRunner())

	mcpSrv := core.NewMCPServer(jobMgr, "")
	if err := mcpSrv.Start(*listen); err != nil {
		log.Fatalf("start mcp server: %v", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := mcpSrv.Stop(shutdownCtx); err != nil {
			log.Printf("shutdown mcp server: %v", err)
		}
	}()

	fmt.Printf("ready %s\n", *listen)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
}

type fixtureAgent struct{}

func (a *fixtureAgent) Name() string { return "fixture-agent" }

func (a *fixtureAgent) StartSession(
	_ context.Context,
	_ string,
) (core.AgentSession, error) {
	return &fixtureSession{events: make(chan core.Event, 2)}, nil
}

func (a *fixtureAgent) ListSessions(
	_ context.Context,
) ([]core.AgentSessionInfo, error) {
	return nil, nil
}

func (a *fixtureAgent) Stop() error { return nil }

type fixtureSession struct {
	events chan core.Event
}

func (s *fixtureSession) Send(prompt string, _ []core.ImageAttachment) error {
	go func() {
		time.Sleep(25 * time.Millisecond)
		s.events <- core.Event{
			Type:    core.EventResult,
			Content: renderEchoResult(prompt),
		}
		close(s.events)
	}()
	return nil
}

func (s *fixtureSession) RespondPermission(
	_ string,
	_ core.PermissionResult,
) error {
	return nil
}

func (s *fixtureSession) Events() <-chan core.Event { return s.events }

func (s *fixtureSession) CurrentSessionID() string { return "fixture-session" }

func (s *fixtureSession) Alive() bool { return true }

func (s *fixtureSession) Close() error { return nil }

func renderEchoResult(prompt string) string {
	return fmt.Sprintf(
		"```echo-result\n{\"status\":\"completed\",\"summary\":%q}\n```",
		"fixture completed: "+prompt,
	)
}
