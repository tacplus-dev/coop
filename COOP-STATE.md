# COOP-STATE

The running log for the coop port. Read this first when resuming; the README
describes what the service is, this file says where the work stands and why
things are the way they are.

## 2026-10-24 — slice 1 built (MOCK mode), fleet proof in flight

Scope (Jody, 2026-10-24): the pigeon PROTOCOL, the COOP service and a GENERIC
pigeon host are platform (tacplus-dev). The texas811 pigeon kind and host stay
SeaBeam's, in sonar. Design records: schematic decisions a30-a31 (pigeons) and
a33 (coop, pigeon commit, self-healing scripts).

Decisions made while building:

- **Verbs.** `RequestPigeon`/`DiscardPigeon` are not in tacplus's closed verb
  registry; the RPCs are `CreatePigeon`/`CancelPigeon`/`GetPigeon`.
- **Thread = topic.** `PigeonMessage` is a `(spec.event)` on
  `coop.PigeonMessageBus.stream`, one thread per run, `sender` set from the
  publisher's token (`(spec.principal)`). The coop subscribes to its own topic
  (`(spec.subscribe)` on `CoopState`) to answer `PIGEON_GONE` and, in MOCK
  mode, to be the pigeon. A dog subscribes with `(spec.event_watch)` on its
  Config carrying `method_s2s: {auth_services: ["<dog>"]}` (mailbox's shape);
  the grant is coop's `auth_services ["*"]` on `PigeonMessage`.
- **Run state in kernel, audit in stratum.** `LiveRun` is `CoopState.runs`
  (kernel state); `PigeonRun` is a `(spec.record)` with one row per
  transition (`<run_id>/<seq>`), stored by stratum.
- **MODE_JOBS refused, not faked.** `Jobs Runner` is nil until slice 2; the
  coop says so with FailedPrecondition.
- **mock_result is typed.** `Kind.mock_result` is a `pigeon.FromPigeon`, not
  bytes: a fleet contract is the service proto plus its import closure
  (`pkg/fleet/fleet.go` extractContract, by design), so a protocol file no
  proto imports is not in the contract and the fleet build left
  `shared/pigeon.proto` out. Typing the field puts the protocol in the
  contract and makes the bundle a message, not base64.
- **tacplus fix.** A service subscribing to a topic in its own package got its
  types package imported twice in the generated Go service manager
  (`coop_pb redeclared`). Fixed on registry-and-floor at 53a02dc
  (`markSubscriptionTypeImports`); fleet-seatrials pins that commit.
- **Idempotency is per caller.** Same caller + same `idempotency_key` returns
  the same run; another caller's key is another run.
- **A ping's run ends with the ping.** tenantecho cancels the run when
  PingPigeon returns, so a proof cannot fill the tenant's live cap.

Proof path: `seatrials pingpigeon --as <tenant pair>` → gateway →
tenantecho-<tenant>.PingPigeon → coop.CreatePigeon (S2S) → TO_PIGEON task on
the thread → coop (mock pigeon) answers Ready then the kind's mock_result
("pong") → tenantecho returns it.

Fleet: coop joined fleet-seatrials (`deploy/bootstrap/services/coop.yaml`,
MODE_MOCK, kind echo.ping). Images coop, tenantecho, kernel, eventbus,
stratum, gateway rebuilt for the new topic/record/Config/RPC.

Next:
- Prove the round trip on seatrials and record the result here.
- Slice 2: MODE_JOBS — client-go Job per pigeon, MintWorkload identity, the
  lease file (`lease_names` → FailedPrecondition until then), pigeons
  namespace RBAC. Then the generic pigeon host.
