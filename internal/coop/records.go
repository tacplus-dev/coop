// Copyright (c) 2026 Jody Scott
// SPDX-License-Identifier: Apache-2.0

package coop

import (
	"context"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/tacplus-dev/coop/tacplus/chassis/serviceconnection"
	pb "github.com/tacplus-dev/coop/tacplus/types/services/coop"
	query_pb "github.com/tacplus-dev/coop/tacplus/types/vservices/query"
)

// PigeonRun rows are written through stratum's generated record write surface
// (InsertIfAbsent<T>) and read back through Get<T>: stratum ingests nothing
// from the event bus (tacplus-dev/stratum#2). Stratum is one instance per
// tenant, so every call names the run's tenant in target_tenant
// (tacplus-dev/tacplus#207). A write is answered once that tenant's redo has
// it at PigeonRun's declared durability (LOCAL; the requests ask for no other,
// so the store holds them to it).

// RunRecords is the record plane's S2S half over PigeonRun, as the generated
// RecordMapClient implements it; the run's tenant's stratum serves it.
type RunRecords interface {
	InsertIfAbsentPigeonRun(ctx context.Context, req *query_pb.InsertIfAbsentPigeonRunRequest, opts ...grpc.CallOption) (*query_pb.RecordWriteResult, error)
	GetPigeonRun(ctx context.Context, req *query_pb.GetPigeonRunRequest, opts ...grpc.CallOption) (*query_pb.GetPigeonRunResponse, error)
}

// runTenant is the tid of the tenant whose stratum holds a run's records: the
// tenant of the dog that asked for it, read from its SPIFFE subject
// (/tenants/<tid>/services/<dog>). Its namespace claim names the tenant by
// name (kernel#23), so the tid is never read from there.
func runTenant(callerSpiffeID string) (string, bool) {
	return serviceconnection.InstanceTenant(callerSpiffeID)
}

// record inserts rec, one transition of a run, into its tenant's stratum,
// under rec's trace_id. The row is keyed <run_id>/<seq>, so a transition
// retried after its row landed finds it there (ALREADY_EXISTS): the same
// transition is done, and another one (a concurrent writer took the seq) is
// ABORTED, so the caller reads the run again.
func (s *Server) record(ctx context.Context, rec *pb.PigeonRun) error {
	tid, ok := runTenant(rec.GetCallerSpiffeId())
	if !ok {
		return status.Errorf(codes.FailedPrecondition, "caller %s names no tenant whose stratum could hold run %s", rec.GetCallerSpiffeId(), rec.GetRunId())
	}
	_, err := s.Records.InsertIfAbsentPigeonRun(ctx, &query_pb.InsertIfAbsentPigeonRunRequest{
		Item: rec, TargetTenant: tid, TraceId: rec.GetTraceId(),
	})
	if status.Code(err) != codes.AlreadyExists {
		return err
	}
	got, err := s.Records.GetPigeonRun(ctx, &query_pb.GetPigeonRunRequest{
		RecordId: rec.GetRecordId(), TenantNamespace: rec.GetTenantNamespace(), TargetTenant: tid, TraceId: rec.GetTraceId(),
	})
	if err != nil {
		return fmt.Errorf("reading PigeonRun %s back: %w", rec.GetRecordId(), err)
	}
	if prev := got.GetItem(); prev.GetState() != rec.GetState() || prev.GetEndReason() != rec.GetEndReason() {
		return status.Errorf(codes.Aborted, "run %s changed underneath: %s is already recorded as %s",
			rec.GetRunId(), rec.GetRecordId(), prev.GetState())
	}
	return nil
}

// storeError is the status a handler answers with when recording a run
// failed: UNAVAILABLE (the redo is unreachable, nothing was written; try
// again), FAILED_PRECONDITION (a durability the store cannot give yet, or a
// caller in no tenant) and ABORTED (the run changed underneath) reach the
// caller as they are; anything else is UNAVAILABLE.
func storeError(err error, what string) error {
	code := codes.Unavailable
	switch c := status.Code(err); c {
	case codes.FailedPrecondition, codes.Aborted:
		code = c
	}
	return status.Errorf(code, "%s: %v", what, err)
}
