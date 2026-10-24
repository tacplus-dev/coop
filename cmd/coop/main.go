// Copyright (c) 2026 Jody Scott
// SPDX-License-Identifier: Apache-2.0

// coop is the fleet's one door to pigeons: tenant services ask it for a
// pigeon, talk to the pigeon on a thread of the PigeonMessage topic, and the
// coop keeps the run's lifecycle in kernel state and on the PigeonRun record.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"go.uber.org/zap"

	"github.com/tacplus-dev/coop/internal/coop"
	loggerchassis "github.com/tacplus-dev/coop/tacplus/chassis/logger"
	"github.com/tacplus-dev/coop/tacplus/chassis/token"
	sm "github.com/tacplus-dev/coop/tacplus/s2sm"
	coop_state "github.com/tacplus-dev/coop/tacplus/s2sm/state/services/coop_state"
	pb "github.com/tacplus-dev/coop/tacplus/types/services/coop"
)

// tickEvery is how often the coop looks for runs past their deadline or
// ready timeout; both are measured in seconds, so this is fine enough.
const tickEvery = 5 * time.Second

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	logger, err := loggerchassis.InstallFromConfig(nil)
	if err != nil {
		return fmt.Errorf("init logger: %w", err)
	}
	zap.ReplaceGlobals(logger)
	defer logger.Sync() //nolint:errcheck

	s, err := sm.New()
	if err != nil {
		return fmt.Errorf("servicemanager: %w", err)
	}
	defer s.Close()

	// The coop's own identity, from the token the fleet minted for it: the
	// sender of every FROM_COOP message and, in MOCK mode, the pigeon.
	raw, err := s.GetToken()
	if err != nil {
		return fmt.Errorf("own token: %w", err)
	}
	own, err := token.ParseToken(raw)
	if err != nil {
		return fmt.Errorf("own token: %w", err)
	}
	if own.GetSubject() == "" {
		return errors.New("own token has no subject")
	}

	server := &coop.Server{
		Publish:    s.CreateCoopPigeonMessageBusStreamPublisher(),
		PublishRun: s.CreateCoopPigeonRunBusStreamPublisher(),
		Runs:       runState{s.StateClients().Services().CoopState()},
		Self:       own.GetSubject(),
		Now:        time.Now,
	}
	// The coop reads its own topic: it answers for pigeons that are gone and,
	// in MOCK mode, it is the pigeon.
	s.SetOnStreamCoopPigeonMessageBusStreamFunc(server.OnMessage)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		t := time.NewTicker(tickEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				server.Tick(ctx)
			}
		}
	}()
	zap.L().Info("coop", zap.String("self", own.GetSubject()))
	return s.Run(server)
}

// runState is coop.RunState over the generated CoopState client: the live
// runs live in kernel state under the run id.
type runState struct{ c *coop_state.StateClient }

func (r runState) Fetch(ctx context.Context, runID string) (*pb.LiveRun, error) {
	return r.c.FetchRuns(ctx, runID)
}
func (r runState) Put(ctx context.Context, run *pb.LiveRun) error {
	_, err := r.c.PutRuns(ctx, run.GetRunId(), run)
	return err
}
func (r runState) All(ctx context.Context) (map[string]*pb.LiveRun, error) {
	return r.c.FetchAllRuns(ctx)
}
