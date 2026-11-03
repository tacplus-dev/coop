# Proposals: coop

Written by loopers. Each proposal: what, why, links. Jody promotes accepted ones into ROADMAP.md (via an agent);
the looper then marks it `promoted` or `declined` here with the date.

## Open
- **<title>** (<date>, <issue/run link>): <what and why>
- **ROADMAP.md: the floor is 2.5, not 2.4** (2026-10-29, #6): the "Now" section still says "tacplus floor 2.4 (declared, #1 still open)" and lists #1. #6 moved `tacplus.yaml` to 2.5 because stratum declares 2.5 and `depends` is transitive; #1's goal is superseded. The issue asks for this line to change; only an agent at Jody's request may edit ROADMAP.md.
- **looper-trace reads none of coop's handlers** (2026-11-03, #7): `looper-trace check .` reports 0 handlers before and after #7. It does not recognise the tacplus Go handler shape `(ctx, *servicemanager.Caller, req)`, the config delivery `CoopConfig(*pb.Config)` or the subscription callback `OnMessage(ctx, *pb.PigeonMessage, *RecordMeta)`, and does not follow helpers (`transition`, `end`, `say`, `gone`, `Runner.Stop`). Until it does, the trace tests in `internal/coop/server_test.go` and `jobs_test.go` are the only check; the gap is looper-trace's (tacplus-dev/looper#51).
- **Finish trace propagation once tacplus#313 ships** (2026-11-03, #7): thread `trace_id` into `RunState` (Fetch, Put, All) and `Runner.Start` (its `WorkloadMintRequest`), with tests at the fake `RunState` and fake Minter, then bump the floor to the tacplus line that has #313.

## Promoted / declined
