// Copyright (c) 2026 Jody Scott
// SPDX-License-Identifier: Apache-2.0

// Package coop is the coop: the fleet's one door to pigeons. This file is the
// handler the generated wrapper serves (auth and validation are the
// wrapper's) plus the run lifecycle; the Kubernetes and identity edges are
// behind Runner, so MOCK mode and the tests need neither.
package coop

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/tacplus-dev/coop/tacplus/chassis/servicemanager"
	sm "github.com/tacplus-dev/coop/tacplus/s2sm"
	fe "github.com/tacplus-dev/coop/tacplus/s2sm/grpc/servers/services/coop"
	eb_pb "github.com/tacplus-dev/coop/tacplus/types/generated/eventbus_support"
	pb "github.com/tacplus-dev/coop/tacplus/types/services/coop"
	pigeon_pb "github.com/tacplus-dev/coop/tacplus/types/shared/pigeon"
)

// RunState is the live runs, in kernel state; the generated StateClient
// satisfies it through runStateClient in main.
type RunState interface {
	Fetch(ctx context.Context, runID string) (*pb.LiveRun, error)
	Put(ctx context.Context, run *pb.LiveRun) error
	All(ctx context.Context) (map[string]*pb.LiveRun, error)
}

// Runner starts and stops the thing that is a pigeon: a Kubernetes Job with
// a minted identity (JobRunner), or nothing at all in MOCK mode. Start returns
// the pigeon's SPIFFE id, which the dog expects FROM_PIGEON messages from.
// Check says whether the pigeon ended on its own, and how; Reconcile, at the
// coop's start, drops what has no live run and returns the live runs that
// have no pigeon.
type Runner interface {
	Start(ctx context.Context, cfg *pb.Config, run *pb.LiveRun, kind *pb.Kind, task []byte) (pigeonSpiffeID string, err error)
	Stop(ctx context.Context, cfg *pb.Config, run *pb.LiveRun) error
	Check(ctx context.Context, cfg *pb.Config, run *pb.LiveRun) (ended bool, reason pb.EndReason, detail string, err error)
	Reconcile(ctx context.Context, cfg *pb.Config, live map[string]*pb.LiveRun) (missing []*pb.LiveRun, err error)
}

// Server is the coop.
type Server struct {
	// Publish is the generated PigeonMessage publisher; PublishRun the
	// generated PigeonRun record publisher. main wires both.
	Publish    func(context.Context, *pb.PigeonMessage) error
	PublishRun func(context.Context, *pb.PigeonRun) error
	Runs       RunState
	// Jobs runs pigeons in MODE_JOBS; nil when the coop has no Kubernetes
	// client, and the mode is then refused rather than pretended.
	Jobs Runner
	// Self is this coop's own SPIFFE id: the sender of FROM_COOP messages and,
	// in MOCK mode, the mock pigeon's identity.
	Self string
	Now  func() time.Time

	mu  sync.RWMutex
	cfg *pb.Config
	// seqs is the next FROM_COOP seq per run, this instance's.
	seqs map[string]int64
	// reconciled: the runner has been reconciled against live runs once.
	reconciled bool
}

var _ sm.Handler = (*Server)(nil)

func (s *Server) CoopGrpcImpl() fe.CoopHandler { return s }

// CoopConfig is the generated config delivery, at start and on change. A
// pause takes effect here: every live run of a paused tenant (or all, when
// the coop is paused) ends KILLED.
func (s *Server) CoopConfig(cfg *pb.Config) error {
	if s.Now == nil {
		s.Now = time.Now
	}
	s.mu.Lock()
	s.cfg = cfg
	if s.seqs == nil {
		s.seqs = map[string]int64{}
	}
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.reconcile(ctx, cfg); err != nil {
		return err
	}
	return s.applyPauses(ctx, cfg)
}

// reconcile, once, on the first config: the runner drops what has no live
// run, and every live run with no pigeon ends FAILED (A30 §3.3, coop restart).
func (s *Server) reconcile(ctx context.Context, cfg *pb.Config) error {
	s.mu.Lock()
	done := s.reconciled
	s.reconciled = true
	s.mu.Unlock()
	if done || cfg.GetMode() != pb.Mode_MODE_JOBS || s.Jobs == nil {
		return nil
	}
	all, err := s.Runs.All(ctx)
	if err != nil {
		return fmt.Errorf("reading live runs: %w", err)
	}
	missing, err := s.Jobs.Reconcile(ctx, cfg, all)
	if err != nil {
		return err
	}
	for _, run := range missing {
		if err := s.end(ctx, run, pb.EndReason_END_REASON_FAILED, "no pigeon after coop restart"); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) CoopStateConfig(*pb.Config) error       { return nil }
func (s *Server) CoopConfigStateConfig(*pb.Config) error { return nil }
func (s *Server) CoopStateGrpcImpl() fe.CoopStateServiceHandler {
	return notHostedState{}
}
func (s *Server) CoopConfigStateGrpcImpl() fe.CoopConfigStateServiceHandler {
	return notHostedConfigState{}
}
func (s *Server) Shutdown(context.Context) {}

func (s *Server) config() *pb.Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// ---------------------------------------------------------------- the RPCs

// callerIdentity is the dog: its SPIFFE subject and its tenant namespace.
func callerIdentity(caller *servicemanager.Caller) (spiffe, tenant string, err error) {
	if caller == nil || caller.Token() == nil {
		return "", "", status.Error(codes.Unauthenticated, "no caller identity")
	}
	spiffe = caller.Token().GetSubject()
	tenant = caller.Token().GetNamespace()
	if spiffe == "" || tenant == "" {
		return "", "", status.Error(codes.Unauthenticated, "caller token names no subject or tenant")
	}
	return spiffe, tenant, nil
}

func (s *Server) CreatePigeon(ctx context.Context, caller *servicemanager.Caller, req *pb.PigeonRequest) (*pb.PigeonTicket, error) {
	dog, tenant, err := callerIdentity(caller)
	if err != nil {
		return nil, err
	}
	cfg := s.config()
	if cfg == nil {
		return nil, status.Error(codes.Unavailable, "coop has no config yet")
	}
	if cfg.GetPaused() {
		return nil, status.Error(codes.FailedPrecondition, "the coop is paused")
	}
	for _, t := range cfg.GetPausedTenants() {
		if t == tenant {
			return nil, status.Errorf(codes.FailedPrecondition, "pigeons are paused for %s", tenant)
		}
	}
	kind := kindNamed(cfg, req.GetKind())
	if kind == nil {
		return nil, status.Errorf(codes.InvalidArgument, "unknown pigeon kind %q", req.GetKind())
	}
	for _, d := range req.GetAllowedDomains() {
		if !domainWithin(d, kind.GetDomainCeiling()) {
			return nil, status.Errorf(codes.InvalidArgument, "domain %q is outside kind %s's ceiling", d, kind.GetName())
		}
	}
	now := s.Now()
	if req.GetDeadlineMs() == 0 {
		return nil, status.Error(codes.InvalidArgument, "deadline_ms is required")
	}
	if req.GetDeadlineMs() <= now.UnixMilli() {
		return nil, status.Error(codes.InvalidArgument, "deadline_ms is in the past")
	}
	if kind.GetMaxDeadlineMs() > 0 && req.GetDeadlineMs() > now.UnixMilli()+kind.GetMaxDeadlineMs() {
		return nil, status.Errorf(codes.InvalidArgument, "deadline exceeds kind %s's maximum of %d ms", kind.GetName(), kind.GetMaxDeadlineMs())
	}
	if kind.GetMaxBudgetMicros() > 0 && req.GetBudgetMicros() > kind.GetMaxBudgetMicros() {
		return nil, status.Errorf(codes.InvalidArgument, "budget exceeds kind %s's maximum of %d micros", kind.GetName(), kind.GetMaxBudgetMicros())
	}
	if len(req.GetLeaseNames()) > 0 {
		return nil, status.Error(codes.FailedPrecondition, "pigeon secrets store not available")
	}
	if cfg.GetMode() == pb.Mode_MODE_UNSPECIFIED {
		return nil, status.Error(codes.FailedPrecondition, "coop config names no mode")
	}
	if cfg.GetMode() == pb.Mode_MODE_JOBS && s.Jobs == nil {
		return nil, status.Error(codes.FailedPrecondition, "MODE_JOBS needs a Kubernetes client; this coop has none")
	}

	all, err := s.Runs.All(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "reading live runs: %v", err)
	}
	live, tenantLive := 0, 0
	for _, r := range all {
		// Idempotency: the same caller with the same key gets the same run.
		if req.GetIdempotencyKey() != "" && r.GetCallerSpiffeId() == dog && r.GetIdempotencyKey() == req.GetIdempotencyKey() {
			return ticket(r), nil
		}
		if r.GetState() != pb.RunState_RUN_STATE_ENDED {
			live++
			if r.GetTenantNamespace() == tenant {
				tenantLive++
			}
		}
	}
	if cfg.GetMaxLiveRuns() > 0 && live >= int(cfg.GetMaxLiveRuns()) {
		return nil, status.Errorf(codes.ResourceExhausted, "the fleet's cap of %d live pigeons is reached", cfg.GetMaxLiveRuns())
	}
	if cfg.GetMaxLiveRunsPerTenant() > 0 && tenantLive >= int(cfg.GetMaxLiveRunsPerTenant()) {
		return nil, status.Errorf(codes.ResourceExhausted, "%s's cap of %d live pigeons is reached", tenant, cfg.GetMaxLiveRunsPerTenant())
	}

	runID := NewRunID()
	run := &pb.LiveRun{
		RunId:           runID,
		TenantNamespace: tenant,
		CallerSpiffeId:  dog,
		Kind:            kind.GetName(),
		ThreadId:        runID,
		State:           pb.RunState_RUN_STATE_REQUESTED,
		DeadlineMs:      req.GetDeadlineMs(),
		CreatedMs:       now.UnixMilli(),
		IdempotencyKey:  req.GetIdempotencyKey(),
	}
	digest := sha256.Sum256(req.GetTask())
	if err := s.transition(ctx, run, pb.RunState_RUN_STATE_REQUESTED, pb.EndReason_END_REASON_UNSPECIFIED, &pb.PigeonRun{
		TaskDigest:     hex.EncodeToString(digest[:]),
		AllowedDomains: req.GetAllowedDomains(),
		BudgetMicros:   req.GetBudgetMicros(),
		LeaseNames:     req.GetLeaseNames(),
	}); err != nil {
		return nil, status.Errorf(codes.Unavailable, "recording the run: %v", err)
	}

	runner := s.runner(cfg)
	pigeonID, err := runner.Start(ctx, cfg, run, kind, req.GetTask())
	if err != nil {
		_ = s.end(ctx, run, pb.EndReason_END_REASON_FAILED, "start failed")
		return nil, status.Errorf(codes.Unavailable, "starting the pigeon: %v", err)
	}
	run.PigeonSpiffeId = pigeonID
	if err := s.transition(ctx, run, pb.RunState_RUN_STATE_STARTING, pb.EndReason_END_REASON_UNSPECIFIED, nil); err != nil {
		return nil, status.Errorf(codes.Unavailable, "recording the run: %v", err)
	}
	return ticket(run), nil
}

func ticket(r *pb.LiveRun) *pb.PigeonTicket {
	return &pb.PigeonTicket{RunId: r.GetRunId(), ThreadId: r.GetThreadId(), PigeonSpiffeId: r.GetPigeonSpiffeId()}
}

func (s *Server) CancelPigeon(ctx context.Context, caller *servicemanager.Caller, req *pb.CancelPigeonRequest) (*pb.CancelPigeonResponse, error) {
	run, err := s.ownRun(ctx, caller, req.GetRunId())
	if err != nil {
		return nil, err
	}
	if run.GetState() == pb.RunState_RUN_STATE_ENDED {
		return &pb.CancelPigeonResponse{}, nil
	}
	reason := pb.EndReason_END_REASON_DONE
	if req.GetReason() == pb.DiscardReason_DISCARD_REASON_KILL {
		reason = pb.EndReason_END_REASON_KILLED
	}
	if err := s.end(ctx, run, reason, strings.ToLower(strings.TrimPrefix(reason.String(), "END_REASON_"))); err != nil {
		return nil, status.Errorf(codes.Unavailable, "ending the run: %v", err)
	}
	return &pb.CancelPigeonResponse{}, nil
}

func (s *Server) GetPigeon(ctx context.Context, caller *servicemanager.Caller, req *pb.GetPigeonRequest) (*pb.PigeonStatus, error) {
	run, err := s.ownRun(ctx, caller, req.GetRunId())
	if err != nil {
		return nil, err
	}
	return &pb.PigeonStatus{
		RunId: run.GetRunId(), ThreadId: run.GetThreadId(), Kind: run.GetKind(), State: run.GetState(),
		EndReason: run.GetEndReason(), PigeonSpiffeId: run.GetPigeonSpiffeId(), DeadlineMs: run.GetDeadlineMs(), CreatedMs: run.GetCreatedMs(),
	}, nil
}

// ownRun is the caller's run, or NotFound: another dog's run does not exist
// as far as it can tell.
func (s *Server) ownRun(ctx context.Context, caller *servicemanager.Caller, runID string) (*pb.LiveRun, error) {
	dog, _, err := callerIdentity(caller)
	if err != nil {
		return nil, err
	}
	run, err := s.Runs.Fetch(ctx, runID)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "reading the run: %v", err)
	}
	if run == nil || run.GetCallerSpiffeId() != dog {
		return nil, status.Errorf(codes.NotFound, "no run %s", runID)
	}
	return run, nil
}

// ---------------------------------------------------------------- lifecycle

// transition moves a run to state, writes it, and puts the transition on the
// record; extra carries the request-time fields the first record keeps.
func (s *Server) transition(ctx context.Context, run *pb.LiveRun, state pb.RunState, reason pb.EndReason, extra *pb.PigeonRun) error {
	run.State = state
	run.EndReason = reason
	run.Seq++
	if err := s.Runs.Put(ctx, run); err != nil {
		return fmt.Errorf("writing live run %s: %w", run.GetRunId(), err)
	}
	rec := &pb.PigeonRun{
		RecordId:        fmt.Sprintf("%s/%d", run.GetRunId(), run.GetSeq()),
		RunId:           run.GetRunId(),
		TenantNamespace: run.GetTenantNamespace(),
		CallerSpiffeId:  run.GetCallerSpiffeId(),
		PigeonSpiffeId:  run.GetPigeonSpiffeId(),
		Kind:            run.GetKind(),
		DeadlineMs:      run.GetDeadlineMs(),
		ThreadId:        run.GetThreadId(),
		State:           state,
		EndReason:       reason,
		JobName:         run.GetJobName(),
		Seq:             run.GetSeq(),
		TsMs:            s.Now().UnixMilli(),
	}
	if extra != nil {
		rec.TaskDigest, rec.AllowedDomains, rec.BudgetMicros, rec.LeaseNames = extra.TaskDigest, extra.AllowedDomains, extra.BudgetMicros, extra.LeaseNames
	}
	if err := s.PublishRun(ctx, rec); err != nil {
		return fmt.Errorf("publishing PigeonRun %s: %w", rec.GetRecordId(), err)
	}
	return nil
}

// end stops the pigeon, records ENDED, and says so on the thread.
func (s *Server) end(ctx context.Context, run *pb.LiveRun, reason pb.EndReason, detail string) error {
	if cfg := s.config(); cfg != nil {
		if err := s.runner(cfg).Stop(ctx, cfg, run); err != nil {
			zap.L().Warn("stopping pigeon", zap.String("run", run.GetRunId()), zap.Error(err))
		}
	}
	if err := s.transition(ctx, run, pb.RunState_RUN_STATE_ENDED, reason, nil); err != nil {
		return err
	}
	return s.say(ctx, run, pb.Direction_DIRECTION_FROM_COOP, nil, &pb.MessageError{Code: pb.MessageErrorCode_MESSAGE_ERROR_CODE_UNSPECIFIED, Detail: "ended: " + detail})
}

// Tick ends every run past its deadline, and every STARTING run the pigeon
// has not reported Ready on within ready_timeout_ms. Called periodically.
func (s *Server) Tick(ctx context.Context) {
	cfg := s.config()
	if cfg == nil {
		return
	}
	all, err := s.Runs.All(ctx)
	if err != nil {
		zap.L().Warn("tick: reading live runs", zap.Error(err))
		return
	}
	now := s.Now().UnixMilli()
	for _, run := range all {
		switch run.GetState() {
		case pb.RunState_RUN_STATE_ENDED:
			continue
		case pb.RunState_RUN_STATE_REQUESTED:
			// CreatePigeon is between recording the run and starting its
			// pigeon; there is no Job to check yet. One that stays here past
			// the ready timeout was abandoned by a coop that died mid-start.
			if cfg.GetReadyTimeoutMs() > 0 && now > run.GetCreatedMs()+cfg.GetReadyTimeoutMs() {
				if err := s.end(ctx, run, pb.EndReason_END_REASON_FAILED, "never started"); err != nil {
					zap.L().Warn("tick: ending run", zap.String("run", run.GetRunId()), zap.Error(err))
				}
			}
			continue
		case pb.RunState_RUN_STATE_STARTING:
			if cfg.GetReadyTimeoutMs() > 0 && now > run.GetCreatedMs()+cfg.GetReadyTimeoutMs() {
				if err := s.end(ctx, run, pb.EndReason_END_REASON_FAILED, "not ready in time"); err != nil {
					zap.L().Warn("tick: ending run", zap.String("run", run.GetRunId()), zap.Error(err))
				}
				continue
			}
		}
		if now > run.GetDeadlineMs() {
			if err := s.end(ctx, run, pb.EndReason_END_REASON_DEADLINE, "deadline"); err != nil {
				zap.L().Warn("tick: ending run", zap.String("run", run.GetRunId()), zap.Error(err))
			}
			continue
		}
		// A pigeon that ended on its own: its Job failed, or completed.
		ended, reason, detail, err := s.runner(cfg).Check(ctx, cfg, run)
		if err != nil {
			zap.L().Warn("tick: checking run", zap.String("run", run.GetRunId()), zap.Error(err))
			continue
		}
		if ended {
			if err := s.end(ctx, run, reason, detail); err != nil {
				zap.L().Warn("tick: ending run", zap.String("run", run.GetRunId()), zap.Error(err))
			}
		}
	}
}

func (s *Server) applyPauses(ctx context.Context, cfg *pb.Config) error {
	if !cfg.GetPaused() && len(cfg.GetPausedTenants()) == 0 {
		return nil
	}
	all, err := s.Runs.All(ctx)
	if err != nil {
		return fmt.Errorf("reading live runs: %w", err)
	}
	paused := map[string]bool{}
	for _, t := range cfg.GetPausedTenants() {
		paused[t] = true
	}
	for _, run := range all {
		if run.GetState() == pb.RunState_RUN_STATE_ENDED {
			continue
		}
		if cfg.GetPaused() || paused[run.GetTenantNamespace()] {
			if err := s.end(ctx, run, pb.EndReason_END_REASON_KILLED, "paused"); err != nil {
				return err
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------- the topic

// OnMessage is the coop's subscription to its own topic: it answers for
// pigeons that are gone, and in MOCK mode it is the pigeon.
func (s *Server) OnMessage(ctx context.Context, m *pb.PigeonMessage, _ *eb_pb.RecordMeta) error {
	if m.GetDirection() != pb.Direction_DIRECTION_TO_PIGEON {
		return nil // never answer FROM_* messages, so it cannot loop
	}
	run, err := s.Runs.Fetch(ctx, m.GetRunId())
	if err != nil {
		return fmt.Errorf("reading run %s: %w", m.GetRunId(), err)
	}
	if run == nil {
		return s.gone(ctx, m, "never_created")
	}
	if m.GetSender() != run.GetCallerSpiffeId() {
		zap.L().Warn("dropping message from a sender that is not the run's caller",
			zap.String("run", run.GetRunId()), zap.String("sender", m.GetSender()))
		return nil
	}
	switch run.GetState() {
	case pb.RunState_RUN_STATE_STARTING, pb.RunState_RUN_STATE_RUNNING:
	default:
		return s.gone(ctx, m, goneDetail(run.GetEndReason()))
	}
	if cfg := s.config(); cfg != nil && cfg.GetMode() == pb.Mode_MODE_MOCK {
		return s.mockPigeon(ctx, run, m, kindNamed(cfg, run.GetKind()))
	}
	return nil
}

func goneDetail(reason pb.EndReason) string {
	switch reason {
	case pb.EndReason_END_REASON_DONE:
		return "discarded"
	case pb.EndReason_END_REASON_DEADLINE:
		return "expired"
	case pb.EndReason_END_REASON_KILLED:
		return "killed"
	default:
		return "failed"
	}
}

func (s *Server) gone(ctx context.Context, m *pb.PigeonMessage, detail string) error {
	return s.Publish(ctx, &pb.PigeonMessage{
		RunId: m.GetRunId(), ThreadId: m.GetThreadId(), Direction: pb.Direction_DIRECTION_FROM_COOP, Kind: m.GetKind(),
		Seq: s.nextSeq(m.GetRunId()), TenantNamespace: m.GetTenantNamespace(), SentMs: s.Now().UnixMilli(), Sender: s.Self,
		Error: &pb.MessageError{Code: pb.MessageErrorCode_MESSAGE_ERROR_CODE_PIGEON_GONE, Detail: detail},
	})
}

// say publishes on a run's thread as the coop (or, in MOCK mode, as the pigeon).
func (s *Server) say(ctx context.Context, run *pb.LiveRun, dir pb.Direction, body proto.Message, e *pb.MessageError) error {
	var raw []byte
	if body != nil {
		var err error
		if raw, err = proto.Marshal(body); err != nil {
			return err
		}
	}
	return s.Publish(ctx, &pb.PigeonMessage{
		RunId: run.GetRunId(), ThreadId: run.GetThreadId(), Direction: dir, Kind: run.GetKind(),
		Seq: s.nextSeq(run.GetRunId()), Body: raw, Error: e, TenantNamespace: run.GetTenantNamespace(),
		SentMs: s.Now().UnixMilli(), Sender: s.Self,
	})
}

func (s *Server) nextSeq(runID string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seqs == nil {
		s.seqs = map[string]int64{}
	}
	s.seqs[runID]++
	return s.seqs[runID]
}

// mockPigeon answers a task with the kind's mock_result and ignores the rest,
// marking the run RUNNING first as a real pigeon's Ready would.
func (s *Server) mockPigeon(ctx context.Context, run *pb.LiveRun, m *pb.PigeonMessage, kind *pb.Kind) error {
	if kind == nil {
		return s.gone(ctx, m, "failed") // the kind left the config under a live run
	}
	var to pigeon_pb.ToPigeon
	if err := proto.Unmarshal(m.GetBody(), &to); err != nil {
		return nil // not ours to judge; a real pigeon would fail the task
	}
	if to.GetTask() == nil {
		return nil
	}
	if run.GetState() == pb.RunState_RUN_STATE_STARTING {
		if err := s.say(ctx, run, pb.Direction_DIRECTION_FROM_PIGEON, &pigeon_pb.FromPigeon{Body: &pigeon_pb.FromPigeon_Ready{Ready: &pigeon_pb.Ready{}}}, nil); err != nil {
			return err
		}
		if err := s.transition(ctx, run, pb.RunState_RUN_STATE_RUNNING, pb.EndReason_END_REASON_UNSPECIFIED, nil); err != nil {
			return err
		}
	}
	var from *pigeon_pb.FromPigeon
	if kind.GetMockResult() != nil {
		from = proto.Clone(kind.GetMockResult()).(*pigeon_pb.FromPigeon)
	} else {
		from = &pigeon_pb.FromPigeon{Body: &pigeon_pb.FromPigeon_Result{Result: &pigeon_pb.PigeonResult{
			Kind: run.GetKind(), ModeUsed: pigeon_pb.RunMode_RUN_MODE_SCRIPT, Status: pigeon_pb.ResultStatus_RESULT_STATUS_OK,
		}}}
	}
	if to.GetTask().GetKind() != run.GetKind() {
		from = &pigeon_pb.FromPigeon{Body: &pigeon_pb.FromPigeon_Result{Result: &pigeon_pb.PigeonResult{
			Kind: run.GetKind(), ModeUsed: pigeon_pb.RunMode_RUN_MODE_SCRIPT, Status: pigeon_pb.ResultStatus_RESULT_STATUS_FAILED,
			Failure: &pigeon_pb.Failure{Code: pigeon_pb.FailureCode_FAILURE_CODE_INVALID_TASK, Detail: "task names kind " + to.GetTask().GetKind()},
		}}}
	}
	return s.say(ctx, run, pb.Direction_DIRECTION_FROM_PIGEON, from, nil)
}

// ---------------------------------------------------------------- runners

func (s *Server) runner(cfg *pb.Config) Runner {
	if cfg.GetMode() == pb.Mode_MODE_JOBS && s.Jobs != nil {
		return s.Jobs
	}
	return mockRunner{self: s.Self}
}

// mockRunner is MODE_MOCK: no Job, no identity; the coop itself is the
// pigeon, so the pigeon's SPIFFE id is the coop's.
type mockRunner struct{ self string }

func (r mockRunner) Start(context.Context, *pb.Config, *pb.LiveRun, *pb.Kind, []byte) (string, error) {
	return r.self, nil
}
func (r mockRunner) Stop(context.Context, *pb.Config, *pb.LiveRun) error { return nil }
func (r mockRunner) Check(context.Context, *pb.Config, *pb.LiveRun) (bool, pb.EndReason, string, error) {
	return false, pb.EndReason_END_REASON_UNSPECIFIED, "", nil
}
func (r mockRunner) Reconcile(context.Context, *pb.Config, map[string]*pb.LiveRun) ([]*pb.LiveRun, error) {
	return nil, nil
}

// ---------------------------------------------------------------- helpers

func kindNamed(cfg *pb.Config, name string) *pb.Kind {
	for _, k := range cfg.GetKinds() {
		if k.GetName() == name {
			return k
		}
	}
	return nil
}

// domainWithin: exact, or under a "*.suffix" entry of the ceiling.
func domainWithin(d string, ceiling []string) bool {
	d = strings.ToLower(d)
	for _, c := range ceiling {
		c = strings.ToLower(c)
		if c == d {
			return true
		}
		if strings.HasPrefix(c, "*.") && strings.HasSuffix(d, c[1:]) && len(d) > len(c)-1 {
			return true
		}
	}
	return false
}

var errNotHosted = status.Error(codes.Unimplemented, "the coop's state is served by the config store (kernel), not by the coop")

// notHostedState: CoopState is a kernel-hosted root; the generated hook is
// satisfied by refusing, the same shape every Go fleet service carries.
type notHostedState struct{}

func (notHostedState) GetRuns(context.Context, *servicemanager.Caller, *pb.RunsGetRequest) (*pb.RunsValueResponse, error) {
	return nil, errNotHosted
}
func (notHostedState) PutRuns(context.Context, *servicemanager.Caller, *pb.RunsPutRequest) (*pb.RunsValueResponse, error) {
	return nil, errNotHosted
}
func (notHostedState) SetRuns(context.Context, *servicemanager.Caller, *pb.RunsPutRequest) (*pb.MapEmpty, error) {
	return nil, errNotHosted
}
func (notHostedState) RemoveRuns(context.Context, *servicemanager.Caller, *pb.RunsGetRequest) (*pb.RunsValueResponse, error) {
	return nil, errNotHosted
}
func (notHostedState) DeleteRuns(context.Context, *servicemanager.Caller, *pb.RunsGetRequest) (*pb.MapEmpty, error) {
	return nil, errNotHosted
}
func (notHostedState) ContainsKeyRuns(context.Context, *servicemanager.Caller, *pb.RunsGetRequest) (*pb.RunsValueResponse, error) {
	return nil, errNotHosted
}
func (notHostedState) GetAllRuns(context.Context, *servicemanager.Caller, *pb.RunsGetAllRequest) (*pb.RunsEntriesResponse, error) {
	return nil, errNotHosted
}
func (notHostedState) KeySetRuns(context.Context, *servicemanager.Caller, *pb.RunsKeysRequest) (*pb.RunsKeysResponse, error) {
	return nil, errNotHosted
}
func (notHostedState) SizeRuns(context.Context, *servicemanager.Caller, *pb.RunsSizeRequest) (*pb.RunsSizeResponse, error) {
	return nil, errNotHosted
}
func (notHostedState) ClearRuns(context.Context, *servicemanager.Caller, *pb.RunsClearRequest) (*pb.MapEmpty, error) {
	return nil, errNotHosted
}
func (notHostedState) PutIfAbsentRuns(context.Context, *servicemanager.Caller, *pb.RunsPutRequest) (*pb.RunsValueResponse, error) {
	return nil, errNotHosted
}
func (notHostedState) WatchRuns(*servicemanager.Caller, grpc.ServerStreamingServer[pb.RunsEntriesResponse]) error {
	return errNotHosted
}
func (notHostedState) QueryRuns(context.Context, *servicemanager.Caller, *pb.RunsQueryRequest) (*pb.RunsQueryResponse, error) {
	return nil, errNotHosted
}
func (notHostedState) WatchQueryRuns(*servicemanager.Caller, *pb.RunsQueryRequest, grpc.ServerStreamingServer[pb.RunsQueryResponse]) error {
	return errNotHosted
}

type notHostedConfigState struct{}

func (notHostedConfigState) GetConfig(context.Context, *servicemanager.Caller) (*pb.Config, error) {
	return nil, errNotHosted
}
func (notHostedConfigState) SetConfig(context.Context, *servicemanager.Caller, *pb.Config) (*pb.Config, error) {
	return nil, errNotHosted
}
func (notHostedConfigState) WatchConfig(*servicemanager.Caller, grpc.ServerStreamingServer[pb.Config]) error {
	return errNotHosted
}
