// Copyright (c) 2026 Jody Scott
// SPDX-License-Identifier: Apache-2.0

package coop

import (
	"context"
	"iter"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/tacplus-dev/coop/tacplus/chassis/serviceconnection"
	pb "github.com/tacplus-dev/coop/tacplus/types/services/coop"
	query_pb "github.com/tacplus-dev/coop/tacplus/types/vservices/query"
)

// fakeRecords is the record plane over PigeonRun in memory, one store per
// tenant as stratum is, with stratum's answers: ALREADY_EXISTS for a key
// already held, NOT_FOUND for one that is not, and UNAVAILABLE while the
// redo is unreachable (nothing is written then). refuse, when set, is the
// answer to every write (FAILED_PRECONDITION for a durability the redo cannot
// give yet, say).
type fakeRecords struct {
	mu          sync.Mutex
	rows        map[string]map[string]*pb.PigeonRun // tid → record_id → row
	log         map[string][]*pb.PigeonRun          // tid → committed writes, in order
	unreachable bool
	refuse      codes.Code
	// inserts and gets are every request that reached the store.
	inserts []*query_pb.InsertIfAbsentPigeonRunRequest
	gets    []*query_pb.GetPigeonRunRequest
	// onInsert sees every row written.
	onInsert func(*pb.PigeonRun)
}

var _ RunRecords = (*fakeRecords)(nil)

func newFakeRecords() *fakeRecords {
	return &fakeRecords{rows: map[string]map[string]*pb.PigeonRun{}, log: map[string][]*pb.PigeonRun{}}
}

// route is the generated client's check: the request names a tid.
func route(tid string) error {
	_, err := serviceconnection.RouteTenant("target_tenant", tid)
	return err
}

func (f *fakeRecords) InsertIfAbsentPigeonRun(_ context.Context, req *query_pb.InsertIfAbsentPigeonRunRequest, _ ...grpc.CallOption) (*query_pb.RecordWriteResult, error) {
	if err := route(req.GetTargetTenant()); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inserts = append(f.inserts, proto.Clone(req).(*query_pb.InsertIfAbsentPigeonRunRequest))
	if f.unreachable {
		return nil, status.Error(codes.Unavailable, "redo unreachable")
	}
	if f.refuse != codes.OK {
		return nil, status.Error(f.refuse, "refused")
	}
	tid, item := req.GetTargetTenant(), req.GetItem()
	if f.rows[tid] == nil {
		f.rows[tid] = map[string]*pb.PigeonRun{}
	}
	if _, ok := f.rows[tid][item.GetRecordId()]; ok {
		return nil, status.Errorf(codes.AlreadyExists, "%s exists", item.GetRecordId())
	}
	row := proto.Clone(item).(*pb.PigeonRun)
	f.rows[tid][item.GetRecordId()] = row
	f.log[tid] = append(f.log[tid], row)
	if f.onInsert != nil {
		f.onInsert(row)
	}
	return &query_pb.RecordWriteResult{Revision: int64(len(f.log[tid])), Durability: query_pb.RecordDurability_RECORD_DURABILITY_LOCAL}, nil
}

func (f *fakeRecords) GetPigeonRun(_ context.Context, req *query_pb.GetPigeonRunRequest, _ ...grpc.CallOption) (*query_pb.GetPigeonRunResponse, error) {
	if err := route(req.GetTargetTenant()); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets = append(f.gets, proto.Clone(req).(*query_pb.GetPigeonRunRequest))
	if f.unreachable {
		return nil, status.Error(codes.Unavailable, "redo unreachable")
	}
	row, ok := f.rows[req.GetTargetTenant()][req.GetRecordId()]
	if !ok || row.GetTenantNamespace() != req.GetTenantNamespace() {
		return nil, status.Errorf(codes.NotFound, "no %s", req.GetRecordId())
	}
	return &query_pb.GetPigeonRunResponse{Item: proto.Clone(row).(*pb.PigeonRun)}, nil
}

// WatchPigeonRunChanges is the change feed of tid's committed writes in the
// request's namespace, from the start.
func (f *fakeRecords) WatchPigeonRunChanges(_ context.Context, req *query_pb.WatchPigeonRunChangesRequest, _ ...grpc.CallOption) (iter.Seq2[*query_pb.PigeonRunWatchEvent, error], error) {
	if err := route(req.GetTargetTenant()); err != nil {
		return nil, err
	}
	f.mu.Lock()
	var events []*query_pb.PigeonRunWatchEvent
	for _, row := range f.log[req.GetTargetTenant()] {
		if row.GetTenantNamespace() != req.GetTenantNamespace() {
			continue
		}
		events = append(events, &query_pb.PigeonRunWatchEvent{Event: &query_pb.PigeonRunWatchEvent_Delta{Delta: &query_pb.PigeonRunWatchChange{
			Item: proto.Clone(row).(*pb.PigeonRun),
		}}})
	}
	f.mu.Unlock()
	return func(yield func(*query_pb.PigeonRunWatchEvent, error) bool) {
		for _, e := range events {
			if !yield(e, nil) {
				return
			}
		}
	}, nil
}
