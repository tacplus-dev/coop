// Copyright (c) 2026 Jody Scott
// SPDX-License-Identifier: Apache-2.0

package coop

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/tacplus-dev/coop/tacplus/chassis/servicemanager"
	"github.com/tacplus-dev/coop/tacplus/chassis/token"
	pb "github.com/tacplus-dev/coop/tacplus/types/services/coop"
	pigeon_pb "github.com/tacplus-dev/coop/tacplus/types/shared/pigeon"
	token_pb "github.com/tacplus-dev/coop/tacplus/types/sys/token"
)

const (
	acme    = "/tenants/acme"
	dogAcme = "spiffe://local.dev/tenants/acme/services/tenantecho"
	coopID  = "spiffe://local.dev/system/services/coop"
)

// as is a verified caller: a tenant service's token.
func as(spiffe, tenant string) *servicemanager.Caller {
	tok := token.NewImmutableToken(&token_pb.Token{Payload: map[string]string{}}).SetSubject(spiffe)
	tok.SetNamespace(tenant)
	return servicemanager.NewCaller(tok, "")
}

// fakeRuns is RunState in memory.
type fakeRuns struct {
	mu   sync.Mutex
	runs map[string]*pb.LiveRun
}

func (f *fakeRuns) Fetch(_ context.Context, id string) (*pb.LiveRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.runs[id]
	if !ok {
		return nil, nil
	}
	return proto.Clone(r).(*pb.LiveRun), nil
}
func (f *fakeRuns) Put(_ context.Context, r *pb.LiveRun) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.runs == nil {
		f.runs = map[string]*pb.LiveRun{}
	}
	f.runs[r.GetRunId()] = proto.Clone(r).(*pb.LiveRun)
	return nil
}
func (f *fakeRuns) All(_ context.Context) (map[string]*pb.LiveRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]*pb.LiveRun{}
	for k, v := range f.runs {
		out[k] = proto.Clone(v).(*pb.LiveRun)
	}
	return out, nil
}

type harness struct {
	s       *Server
	runs    *fakeRuns
	msgs    []*pb.PigeonMessage
	records []*pb.PigeonRun
	now     time.Time
}

func newHarness(t *testing.T, cfg *pb.Config) *harness {
	t.Helper()
	h := &harness{runs: &fakeRuns{}, now: time.Unix(1_700_000_000, 0)}
	h.s = &Server{
		Publish:    func(_ context.Context, m *pb.PigeonMessage) error { h.msgs = append(h.msgs, m); return nil },
		PublishRun: func(_ context.Context, r *pb.PigeonRun) error { h.records = append(h.records, r); return nil },
		Runs:       h.runs,
		Self:       coopID,
		Now:        func() time.Time { return h.now },
	}
	if err := h.s.CoopConfig(cfg); err != nil {
		t.Fatalf("CoopConfig: %v", err)
	}
	return h
}

func mockCfg() *pb.Config {
	pong := &pigeon_pb.FromPigeon{Body: &pigeon_pb.FromPigeon_Result{Result: &pigeon_pb.PigeonResult{
		Kind: "echo.ping", Status: pigeon_pb.ResultStatus_RESULT_STATUS_OK, Result: []byte("pong"),
	}}}
	return &pb.Config{
		Mode:                 pb.Mode_MODE_MOCK,
		MaxLiveRuns:          32,
		MaxLiveRunsPerTenant: 2,
		ReadyTimeoutMs:       60_000,
		Kinds: []*pb.Kind{{
			Name: "echo.ping", DomainCeiling: []string{"*.example.com", "api.other.org"},
			MaxDeadlineMs: 600_000, MaxBudgetMicros: 1_000_000, MockResult: pong,
		}},
	}
}

func (h *harness) request(kind string) *pb.PigeonRequest {
	return &pb.PigeonRequest{Kind: kind, DeadlineMs: h.now.Add(time.Minute).UnixMilli(), Task: []byte("task")}
}

// toPigeon is what a dog puts on the thread: a ToPigeon task, as the caller.
func (h *harness) toPigeon(tk *pb.PigeonTicket, sender, taskKind string) *pb.PigeonMessage {
	body, _ := proto.Marshal(&pigeon_pb.ToPigeon{Body: &pigeon_pb.ToPigeon_Task{Task: &pigeon_pb.PigeonTask{Kind: taskKind, JobId: "j1"}}})
	return &pb.PigeonMessage{RunId: tk.GetRunId(), ThreadId: tk.GetThreadId(), Direction: pb.Direction_DIRECTION_TO_PIGEON,
		Kind: "echo.ping", Seq: 1, Body: body, TenantNamespace: acme, Sender: sender}
}

func wantCode(t *testing.T, err error, code codes.Code, contains string) {
	t.Helper()
	if status.Code(err) != code {
		t.Fatalf("want %v, got %v", code, err)
	}
	if !strings.Contains(err.Error(), contains) {
		t.Fatalf("want %q in %v", contains, err)
	}
}

func TestCreatePigeonValidations(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, mockCfg())
	dog := as(dogAcme, acme)

	_, err := h.s.CreatePigeon(ctx, nil, h.request("echo.ping"))
	wantCode(t, err, codes.Unauthenticated, "no caller identity")

	_, err = h.s.CreatePigeon(ctx, dog, h.request("nope"))
	wantCode(t, err, codes.InvalidArgument, `unknown pigeon kind "nope"`)

	r := h.request("echo.ping")
	r.AllowedDomains = []string{"a.example.com", "evil.org"}
	_, err = h.s.CreatePigeon(ctx, dog, r)
	wantCode(t, err, codes.InvalidArgument, `domain "evil.org" is outside`)

	r = h.request("echo.ping")
	r.DeadlineMs = 0
	_, err = h.s.CreatePigeon(ctx, dog, r)
	wantCode(t, err, codes.InvalidArgument, "deadline_ms is required")

	r.DeadlineMs = h.now.Add(-time.Second).UnixMilli()
	_, err = h.s.CreatePigeon(ctx, dog, r)
	wantCode(t, err, codes.InvalidArgument, "in the past")

	r.DeadlineMs = h.now.Add(time.Hour).UnixMilli()
	_, err = h.s.CreatePigeon(ctx, dog, r)
	wantCode(t, err, codes.InvalidArgument, "exceeds kind echo.ping's maximum")

	r = h.request("echo.ping")
	r.BudgetMicros = 2_000_000
	_, err = h.s.CreatePigeon(ctx, dog, r)
	wantCode(t, err, codes.InvalidArgument, "budget exceeds")

	r = h.request("echo.ping")
	r.LeaseNames = []string{"gmail"}
	_, err = h.s.CreatePigeon(ctx, dog, r)
	wantCode(t, err, codes.FailedPrecondition, "secrets store not available")

	cfg := mockCfg()
	cfg.Mode = pb.Mode_MODE_JOBS
	hj := newHarness(t, cfg)
	_, err = hj.s.CreatePigeon(ctx, dog, hj.request("echo.ping"))
	wantCode(t, err, codes.FailedPrecondition, "MODE_JOBS is not built yet")

	cfg = mockCfg()
	cfg.Paused = true
	hp := newHarness(t, cfg)
	_, err = hp.s.CreatePigeon(ctx, dog, hp.request("echo.ping"))
	wantCode(t, err, codes.FailedPrecondition, "coop is paused")

	cfg = mockCfg()
	cfg.PausedTenants = []string{acme}
	hp = newHarness(t, cfg)
	_, err = hp.s.CreatePigeon(ctx, dog, hp.request("echo.ping"))
	wantCode(t, err, codes.FailedPrecondition, "paused for /tenants/acme")
}

func TestCreatePigeonCapsAndIdempotency(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, mockCfg())
	dog := as(dogAcme, acme)

	r := h.request("echo.ping")
	r.IdempotencyKey = "k1"
	first, err := h.s.CreatePigeon(ctx, dog, r)
	if err != nil {
		t.Fatal(err)
	}
	again, err := h.s.CreatePigeon(ctx, dog, r)
	if err != nil {
		t.Fatal(err)
	}
	if again.GetRunId() != first.GetRunId() {
		t.Fatalf("same caller + key must return the same run: %s vs %s", again.GetRunId(), first.GetRunId())
	}
	// Another dog with the same key is a different run.
	other, err := h.s.CreatePigeon(ctx, as("spiffe://local.dev/tenants/acme/services/other", acme), r)
	if err != nil {
		t.Fatal(err)
	}
	if other.GetRunId() == first.GetRunId() {
		t.Fatal("idempotency keys are per caller")
	}
	// The tenant cap is 2 and both are live.
	_, err = h.s.CreatePigeon(ctx, dog, h.request("echo.ping"))
	wantCode(t, err, codes.ResourceExhausted, "/tenants/acme's cap of 2")

	// A ticket names the mock pigeon as the coop itself and a thread = run id.
	if first.GetPigeonSpiffeId() != coopID || first.GetThreadId() != first.GetRunId() {
		t.Fatalf("ticket %v", first)
	}
	// Two records so far per run: REQUESTED then STARTING, the first carrying the task digest.
	if len(h.records) != 4 || h.records[0].GetState() != pb.RunState_RUN_STATE_REQUESTED || h.records[0].GetTaskDigest() == "" ||
		h.records[1].GetState() != pb.RunState_RUN_STATE_STARTING || h.records[1].GetRecordId() != first.GetRunId()+"/2" {
		t.Fatalf("records %v", h.records)
	}
}

func TestMockRoundTrip(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, mockCfg())
	dog := as(dogAcme, acme)
	tk, err := h.s.CreatePigeon(ctx, dog, h.request("echo.ping"))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.s.OnMessage(ctx, h.toPigeon(tk, dogAcme, "echo.ping"), nil); err != nil {
		t.Fatal(err)
	}
	if len(h.msgs) != 2 {
		t.Fatalf("want Ready then Result, got %d messages", len(h.msgs))
	}
	var ready, result pigeon_pb.FromPigeon
	if err := proto.Unmarshal(h.msgs[0].GetBody(), &ready); err != nil || ready.GetReady() == nil {
		t.Fatalf("first message is not Ready: %v %v", err, h.msgs[0])
	}
	if err := proto.Unmarshal(h.msgs[1].GetBody(), &result); err != nil || string(result.GetResult().GetResult()) != "pong" {
		t.Fatalf("second message is not the mock result: %v %v", err, h.msgs[1])
	}
	for i, m := range h.msgs {
		if m.GetDirection() != pb.Direction_DIRECTION_FROM_PIGEON || m.GetSender() != coopID || m.GetSeq() != int64(i+1) || m.GetTenantNamespace() != acme {
			t.Fatalf("message %d: %v", i, m)
		}
	}
	st, err := h.s.GetPigeon(ctx, dog, &pb.GetPigeonRequest{RunId: tk.GetRunId()})
	if err != nil || st.GetState() != pb.RunState_RUN_STATE_RUNNING {
		t.Fatalf("after Ready the run is RUNNING: %v %v", st, err)
	}

	// A task naming another kind fails with INVALID_TASK; the run stays RUNNING.
	if err := h.s.OnMessage(ctx, h.toPigeon(tk, dogAcme, "other.kind"), nil); err != nil {
		t.Fatal(err)
	}
	var bad pigeon_pb.FromPigeon
	_ = proto.Unmarshal(h.msgs[len(h.msgs)-1].GetBody(), &bad)
	if bad.GetResult().GetFailure().GetCode() != pigeon_pb.FailureCode_FAILURE_CODE_INVALID_TASK {
		t.Fatalf("want INVALID_TASK, got %v", bad.GetResult())
	}

	// FROM_* messages are never answered: no loop.
	n := len(h.msgs)
	m := h.toPigeon(tk, dogAcme, "echo.ping")
	m.Direction = pb.Direction_DIRECTION_FROM_PIGEON
	if err := h.s.OnMessage(ctx, m, nil); err != nil || len(h.msgs) != n {
		t.Fatalf("FROM_PIGEON must be ignored: %v %d", err, len(h.msgs)-n)
	}

	// Cancel ends the run DONE and says so on the thread; cancelling again is a no-op.
	if _, err := h.s.CancelPigeon(ctx, dog, &pb.CancelPigeonRequest{RunId: tk.GetRunId()}); err != nil {
		t.Fatal(err)
	}
	last := h.msgs[len(h.msgs)-1]
	if last.GetDirection() != pb.Direction_DIRECTION_FROM_COOP || last.GetError().GetDetail() != "ended: done" {
		t.Fatalf("end message %v", last)
	}
	st, _ = h.s.GetPigeon(ctx, dog, &pb.GetPigeonRequest{RunId: tk.GetRunId()})
	if st.GetState() != pb.RunState_RUN_STATE_ENDED || st.GetEndReason() != pb.EndReason_END_REASON_DONE {
		t.Fatalf("status %v", st)
	}
	n = len(h.msgs)
	if _, err := h.s.CancelPigeon(ctx, dog, &pb.CancelPigeonRequest{RunId: tk.GetRunId()}); err != nil || len(h.msgs) != n {
		t.Fatalf("second cancel: %v %d", err, len(h.msgs)-n)
	}
	// The thread of an ended run answers PIGEON_GONE discarded.
	if err := h.s.OnMessage(ctx, h.toPigeon(tk, dogAcme, "echo.ping"), nil); err != nil {
		t.Fatal(err)
	}
	last = h.msgs[len(h.msgs)-1]
	if last.GetError().GetCode() != pb.MessageErrorCode_MESSAGE_ERROR_CODE_PIGEON_GONE || last.GetError().GetDetail() != "discarded" {
		t.Fatalf("gone message %v", last)
	}
}

func TestOwnRunIsPerCaller(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, mockCfg())
	tk, err := h.s.CreatePigeon(ctx, as(dogAcme, acme), h.request("echo.ping"))
	if err != nil {
		t.Fatal(err)
	}
	other := as("spiffe://local.dev/tenants/beta/services/tenantecho", "/tenants/beta")
	_, err = h.s.GetPigeon(ctx, other, &pb.GetPigeonRequest{RunId: tk.GetRunId()})
	wantCode(t, err, codes.NotFound, "no run")
	_, err = h.s.CancelPigeon(ctx, other, &pb.CancelPigeonRequest{RunId: tk.GetRunId()})
	wantCode(t, err, codes.NotFound, "no run")
	_, err = h.s.GetPigeon(ctx, as(dogAcme, acme), &pb.GetPigeonRequest{RunId: "missing"})
	wantCode(t, err, codes.NotFound, "no run missing")
}

func TestOnMessageGoneAndForeignSender(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, mockCfg())

	// No such run: never_created.
	m := h.toPigeon(&pb.PigeonTicket{RunId: "nope", ThreadId: "nope"}, dogAcme, "echo.ping")
	if err := h.s.OnMessage(ctx, m, nil); err != nil {
		t.Fatal(err)
	}
	if got := h.msgs[0].GetError(); got.GetCode() != pb.MessageErrorCode_MESSAGE_ERROR_CODE_PIGEON_GONE || got.GetDetail() != "never_created" || h.msgs[0].GetSender() != coopID {
		t.Fatalf("gone %v", h.msgs[0])
	}

	tk, err := h.s.CreatePigeon(ctx, as(dogAcme, acme), h.request("echo.ping"))
	if err != nil {
		t.Fatal(err)
	}
	// A message from someone other than the run's caller is dropped silently.
	n := len(h.msgs)
	if err := h.s.OnMessage(ctx, h.toPigeon(tk, "spiffe://local.dev/tenants/acme/services/other", "echo.ping"), nil); err != nil || len(h.msgs) != n {
		t.Fatalf("foreign sender: %v %d", err, len(h.msgs)-n)
	}

	// Killed and expired runs answer with their reason.
	if _, err := h.s.CancelPigeon(ctx, as(dogAcme, acme), &pb.CancelPigeonRequest{RunId: tk.GetRunId(), Reason: pb.DiscardReason_DISCARD_REASON_KILL}); err != nil {
		t.Fatal(err)
	}
	_ = h.s.OnMessage(ctx, h.toPigeon(tk, dogAcme, "echo.ping"), nil)
	if d := h.msgs[len(h.msgs)-1].GetError().GetDetail(); d != "killed" {
		t.Fatalf("want killed, got %q", d)
	}
}

func TestTickEndsLateAndUnreadyRuns(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, mockCfg())
	dog := as(dogAcme, acme)
	unready, err := h.s.CreatePigeon(ctx, dog, h.request("echo.ping"))
	if err != nil {
		t.Fatal(err)
	}
	r := h.request("echo.ping")
	r.DeadlineMs = h.now.Add(5 * time.Minute).UnixMilli()
	running, err := h.s.CreatePigeon(ctx, dog, r)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.s.OnMessage(ctx, h.toPigeon(running, dogAcme, "echo.ping"), nil); err != nil {
		t.Fatal(err)
	}

	// Nothing is late yet.
	h.s.Tick(ctx)
	for _, id := range []string{unready.GetRunId(), running.GetRunId()} {
		st, _ := h.s.GetPigeon(ctx, dog, &pb.GetPigeonRequest{RunId: id})
		if st.GetState() == pb.RunState_RUN_STATE_ENDED {
			t.Fatalf("run %s ended early", id)
		}
	}
	// 61s on: the STARTING run missed ready_timeout (60s) and its 60s deadline; the RUNNING one has 5 minutes.
	h.now = h.now.Add(61 * time.Second)
	h.s.Tick(ctx)
	st, _ := h.s.GetPigeon(ctx, dog, &pb.GetPigeonRequest{RunId: unready.GetRunId()})
	if st.GetState() != pb.RunState_RUN_STATE_ENDED || st.GetEndReason() != pb.EndReason_END_REASON_FAILED {
		t.Fatalf("unready run: %v", st)
	}
	st, _ = h.s.GetPigeon(ctx, dog, &pb.GetPigeonRequest{RunId: running.GetRunId()})
	if st.GetState() != pb.RunState_RUN_STATE_RUNNING {
		t.Fatalf("running run: %v", st)
	}
	h.now = h.now.Add(5 * time.Minute)
	h.s.Tick(ctx)
	st, _ = h.s.GetPigeon(ctx, dog, &pb.GetPigeonRequest{RunId: running.GetRunId()})
	if st.GetState() != pb.RunState_RUN_STATE_ENDED || st.GetEndReason() != pb.EndReason_END_REASON_DEADLINE {
		t.Fatalf("expired run: %v", st)
	}
	_ = h.s.OnMessage(ctx, h.toPigeon(running, dogAcme, "echo.ping"), nil)
	if d := h.msgs[len(h.msgs)-1].GetError().GetDetail(); d != "expired" {
		t.Fatalf("want expired, got %q", d)
	}
}

func TestPauseKillsLiveRuns(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, mockCfg())
	dog := as(dogAcme, acme)
	tk, err := h.s.CreatePigeon(ctx, dog, h.request("echo.ping"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := mockCfg()
	cfg.PausedTenants = []string{acme}
	if err := h.s.CoopConfig(cfg); err != nil {
		t.Fatal(err)
	}
	st, _ := h.s.GetPigeon(ctx, dog, &pb.GetPigeonRequest{RunId: tk.GetRunId()})
	if st.GetState() != pb.RunState_RUN_STATE_ENDED || st.GetEndReason() != pb.EndReason_END_REASON_KILLED {
		t.Fatalf("paused tenant's run: %v", st)
	}
}

func TestDomainWithin(t *testing.T) {
	ceiling := []string{"*.example.com", "api.other.org"}
	for d, want := range map[string]bool{
		"a.example.com": true, "A.B.Example.COM": true, "example.com": false, ".example.com": false,
		"api.other.org": true, "x.api.other.org": false, "evil.org": false,
	} {
		if got := domainWithin(d, ceiling); got != want {
			t.Errorf("domainWithin(%q) = %v, want %v", d, got, want)
		}
	}
}

func TestNewRunID(t *testing.T) {
	a, b := NewRunID(), NewRunID()
	if len(a) != 26 || a == b || strings.ToLower(a) != a {
		t.Fatalf("run ids: %q %q", a, b)
	}
}
