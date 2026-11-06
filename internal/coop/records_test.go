// Copyright (c) 2026 Jody Scott
// SPDX-License-Identifier: Apache-2.0

package coop

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/tacplus-dev/coop/tacplus/types/services/coop"
	query_pb "github.com/tacplus-dev/coop/tacplus/types/vservices/query"
)

// Stratum ingests no record from the event bus (stratum#2): PigeonRun, the
// coop's one record type, is written through stratum's write surface (#5).
// The tests below write it through the handlers, read it back, see Watch
// deliver it, and see it refused, nothing written, while the redo is
// unreachable.

// TestNoRecordIsPublishedOnTheBus: the proto publishes no record topic and
// grants stratum none, and no code reaches for a record publisher.
func TestNoRecordIsPublishedOnTheBus(t *testing.T) {
	root := filepath.Join("..", "..")
	src, err := os.ReadFile(filepath.Join(root, "proto", "services", "coop.proto"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(src), `topic_ref: "coop.PigeonRunBus.stream"`) {
		t.Error("coop.proto publishes PigeonRun's topic: stratum would never store what it publishes")
	}
	if strings.Contains(string(src), `auth_services: ["stratum"]`) {
		t.Error("a record still grants stratum its topic: stratum subscribes to none")
	}
	for _, dir := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if strings.Contains(string(b), "PigeonRunBusStreamPublisher") {
				t.Errorf("%s publishes a record on the bus", path)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

// watchedRuns is every PigeonRun Watch<T>Changes delivers for acme from the
// start, in order.
func watchedRuns(t *testing.T, store *fakeRecords) []*pb.PigeonRun {
	t.Helper()
	events, err := store.WatchPigeonRunChanges(context.Background(), &query_pb.WatchPigeonRunChangesRequest{TargetTenant: acmeTid, TenantNamespace: acme})
	if err != nil {
		t.Fatal(err)
	}
	var out []*pb.PigeonRun
	for e, err := range events {
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, e.GetDelta().GetItem())
	}
	return out
}

// TestPigeonRunIsWrittenReadAndWatched: each transition of a run is inserted
// into the stratum of the dog's tenant, routed by its tid, at the declared
// durability; it reads back by key, and Watch delivers it in order.
func TestPigeonRunIsWrittenReadAndWatched(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, mockCfg())
	dog := as(dogAcme, acme)
	tk, err := h.s.CreatePigeon(ctx, dog, h.request("echo.ping"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.CancelPigeon(ctx, dog, &pb.CancelPigeonRequest{RunId: tk.GetRunId(), Reason: pb.DiscardReason_DISCARD_REASON_DONE}); err != nil {
		t.Fatal(err)
	}

	for _, req := range h.store.inserts {
		if req.GetTargetTenant() != acmeTid {
			t.Fatalf("write routed to %q, want acme's tid %q", req.GetTargetTenant(), acmeTid)
		}
		if req.GetDurability() != query_pb.RecordDurability_RECORD_DURABILITY_UNSPECIFIED {
			t.Fatalf("write asks for %v; it is held to PigeonRun's declared durability", req.GetDurability())
		}
	}
	if len(h.store.rows) != 1 {
		t.Fatalf("rows in %d tenants' stores, want acme's only", len(h.store.rows))
	}

	got, err := h.store.GetPigeonRun(ctx, &query_pb.GetPigeonRunRequest{RecordId: tk.GetRunId() + "/1", TenantNamespace: acme, TargetTenant: acmeTid})
	if err != nil {
		t.Fatal(err)
	}
	if r := got.GetItem(); r.GetState() != pb.RunState_RUN_STATE_REQUESTED || r.GetCallerSpiffeId() != dogAcme || r.GetTaskDigest() == "" {
		t.Fatalf("read back %v", r)
	}

	var states []pb.RunState
	for _, r := range watchedRuns(t, h.store) {
		if r.GetRunId() != tk.GetRunId() {
			t.Fatalf("watch delivered another run: %v", r)
		}
		states = append(states, r.GetState())
	}
	want := []pb.RunState{pb.RunState_RUN_STATE_REQUESTED, pb.RunState_RUN_STATE_STARTING, pb.RunState_RUN_STATE_ENDED}
	if len(states) != len(want) {
		t.Fatalf("watch delivered %v, want %v", states, want)
	}
	for i := range want {
		if states[i] != want[i] {
			t.Fatalf("watch delivered %v, want %v", states, want)
		}
	}
}

// TestPigeonRunRefusedWhileRedoUnreachable: stratum refuses every write while
// its redo is unreachable. CreatePigeon then answers UNAVAILABLE with no run
// and no pigeon; CancelPigeon answers UNAVAILABLE and the run stays as it was,
// to be ended by the retry once the redo is back.
func TestPigeonRunRefusedWhileRedoUnreachable(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, mockCfg())
	dog := as(dogAcme, acme)
	h.store.unreachable = true
	_, err := h.s.CreatePigeon(ctx, dog, h.request("echo.ping"))
	wantCode(t, err, codes.Unavailable, "redo unreachable")
	if all, _ := h.runs.All(ctx); len(all) != 0 || len(h.records) != 0 {
		t.Fatalf("a refused run was kept: runs %v, records %v", all, h.records)
	}

	h.store.unreachable = false
	tk, err := h.s.CreatePigeon(ctx, dog, h.request("echo.ping"))
	if err != nil {
		t.Fatal(err)
	}
	h.store.unreachable = true
	n := len(h.records)
	_, err = h.s.CancelPigeon(ctx, dog, &pb.CancelPigeonRequest{RunId: tk.GetRunId(), Reason: pb.DiscardReason_DISCARD_REASON_KILL})
	wantCode(t, err, codes.Unavailable, "redo unreachable")
	if st, _ := h.s.GetPigeon(ctx, dog, &pb.GetPigeonRequest{RunId: tk.GetRunId()}); st.GetState() != pb.RunState_RUN_STATE_STARTING || len(h.records) != n {
		t.Fatalf("a refused cancel changed the run: %v", st)
	}

	h.store.unreachable = false
	if _, err := h.s.CancelPigeon(ctx, dog, &pb.CancelPigeonRequest{RunId: tk.GetRunId(), Reason: pb.DiscardReason_DISCARD_REASON_KILL}); err != nil {
		t.Fatal(err)
	}
	if st, _ := h.s.GetPigeon(ctx, dog, &pb.GetPigeonRequest{RunId: tk.GetRunId()}); st.GetState() != pb.RunState_RUN_STATE_ENDED {
		t.Fatalf("retried cancel: %v", st)
	}
}

// TestPigeonRunRefusalsReachTheCaller: FAILED_PRECONDITION (a durability the
// redo cannot give yet: REMOTE until redo#1 step 2) reaches the dog as it is,
// and so does ABORTED; any other refusal is UNAVAILABLE.
func TestPigeonRunRefusalsReachTheCaller(t *testing.T) {
	ctx := context.Background()
	for refused, want := range map[codes.Code]codes.Code{
		codes.FailedPrecondition: codes.FailedPrecondition,
		codes.Aborted:            codes.Aborted,
		codes.Internal:           codes.Unavailable,
	} {
		h := newHarness(t, mockCfg())
		h.store.refuse = refused
		_, err := h.s.CreatePigeon(ctx, as(dogAcme, acme), h.request("echo.ping"))
		wantCode(t, err, want, "recording the run")
		if all, _ := h.runs.All(ctx); len(all) != 0 {
			t.Fatalf("%v: a refused run was kept: %v", refused, all)
		}
	}
}

// TestPigeonRunNeedsATenant: a caller in no tenant has no stratum to hold its
// run, so it gets none.
func TestPigeonRunNeedsATenant(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, mockCfg())
	_, err := h.s.CreatePigeon(ctx, as("spiffe://local.dev/system/services/someone", "/system"), h.request("echo.ping"))
	wantCode(t, err, codes.FailedPrecondition, "names no tenant")
	if all, _ := h.runs.All(ctx); len(all) != 0 || len(h.store.inserts) != 0 {
		t.Fatalf("a run with no tenant was kept: runs %v, writes %v", all, h.store.inserts)
	}
}

// TestTransitionAlreadyRecorded: a transition retried after its row landed
// (its live run write failed) finds the row by Get and is done; one whose seq
// another writer took is ABORTED, and the live run is left as it was.
func TestTransitionAlreadyRecorded(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, mockCfg())
	run := func() *pb.LiveRun {
		return &pb.LiveRun{RunId: "01dup", ThreadId: "01dup", TenantNamespace: acme, CallerSpiffeId: dogAcme, Kind: "echo.ping",
			State: pb.RunState_RUN_STATE_STARTING, Seq: 2}
	}
	if err := h.runs.Put(ctx, run()); err != nil {
		t.Fatal(err)
	}
	if err := h.s.record(ctx, &pb.PigeonRun{RecordId: "01dup/3", RunId: "01dup", TenantNamespace: acme, CallerSpiffeId: dogAcme,
		State: pb.RunState_RUN_STATE_ENDED, EndReason: pb.EndReason_END_REASON_DONE}); err != nil {
		t.Fatal(err)
	}

	if err := h.s.transition(ctx, run(), pb.RunState_RUN_STATE_ENDED, pb.EndReason_END_REASON_DONE, nil, "trace-retry"); err != nil {
		t.Fatalf("retried transition: %v", err)
	}
	if got, _ := h.runs.Fetch(ctx, "01dup"); got.GetState() != pb.RunState_RUN_STATE_ENDED || got.GetSeq() != 3 {
		t.Fatalf("live run after the retry: %v", got)
	}
	if g := h.store.gets[len(h.store.gets)-1]; g.GetRecordId() != "01dup/3" || g.GetTargetTenant() != acmeTid || g.GetTraceId() != "trace-retry" {
		t.Fatalf("read back with %v", g)
	}

	if err := h.runs.Put(ctx, run()); err != nil {
		t.Fatal(err)
	}
	err := h.s.transition(ctx, run(), pb.RunState_RUN_STATE_ENDED, pb.EndReason_END_REASON_KILLED, nil, "")
	if status.Code(err) != codes.Aborted {
		t.Fatalf("transition over another's row: %v, want ABORTED", err)
	}
	if got, _ := h.runs.Fetch(ctx, "01dup"); got.GetState() != pb.RunState_RUN_STATE_STARTING {
		t.Fatalf("an aborted transition changed the live run: %v", got)
	}
}
