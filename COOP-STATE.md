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
- **tacplus fix 2: Go subscribers mint for the broker.** The generated Go
  subscriber opened its stream on the eventbus connection whose audience
  heuristic reads the method's package, so it minted a token for the topic
  OWNER and eventbus refused it. The subscriber now takes a TokenSource and
  the service manager hands it GetTokenFor(<broker>), as publishers and the
  Rust/Java subscribers already did. registry-and-floor f65ffc2.
- **Delegates.** kernel.yaml needs a `coop` delegate and `coop` in every
  dog's `authorized_services`; the operator renders coop only when
  `topology.app_services.coop` names it.
- **UNSPECIFIED is a value.** The broker enforces `assert_enum_defined` on
  events; `PigeonRun.end_reason` (non-final rows) and `MessageError.code`
  (informational end message) carry UNSPECIFIED legitimately, so neither is
  asserted.
- **sender is self-reported, for now.** The wrapper stamps `(spec.principal)`
  on RPC requests from the caller's token, but nothing stamps it on a
  published event: the dog's message reached the coop with no sender and was
  dropped. The dog sets `sender` from its own token. Honest within a fleet
  (every publisher is a service with a verified publish token), but the real
  fix is the broker stamping `(spec.principal)` from the publish token —
  tacplus/eventbus follow-up.
- **Idempotency is per caller.** Same caller + same `idempotency_key` returns
  the same run; another caller's key is another run.
- **A ping's run ends with the ping.** tenantecho cancels the run (reason
  DONE) when PingPigeon returns, so a proof cannot fill the tenant's live cap.

Proof path: `seatrials pingpigeon --as <tenant pair>` → gateway →
tenantecho-<tenant>.PingPigeon → coop.CreatePigeon (S2S) → TO_PIGEON task on
the thread → coop (mock pigeon) answers Ready then the kind's mock_result
("pong") → tenantecho returns it.

Fleet: coop joined fleet-seatrials (`deploy/bootstrap/services/coop.yaml`,
MODE_MOCK, kind echo.ping). Images coop, tenantecho, kernel, eventbus,
stratum, gateway rebuilt for the new topic/record/Config/RPC.

**Proven 2026-10-25 00:5xZ on seatrials** (fleet-seatrials b632567, coop
a5d4056, tenantecho 794a3ac, tacplus f65ffc2): `seatrials pingpigeon --as
alpha` and `--as beta` each returned result "pong", pigeon
spiffe://local.dev/system/services/coop, served_by the caller's own tenant
instance; the coop log shows one CreatePigeon and one CancelPigeon per ping
and eventbus rejected no events. Two more defects were found and fixed on the
way: CancelPigeonRequest.reason must be a defined value (the dog sent
UNSPECIFIED and swallowed the error, so runs lived to their deadline), and
the four items above.

## 2026-10-25 — slice 2 in flight (Jobs, no internet)

Built, pushed, composing into seatrials:
- **coop JobRunner** (internal/coop/jobs.go): per run, MintWorkload (terminal,
  exp ≤ min(deadline, 300 s)), a Secret `pigeon-<run>` with the bootstrap
  token and the coop's own bootstrap config, a Job `pigeon-<run>` locked down
  per A30 §3.3 (no k8s identity, non-root, read-only root, caps dropped,
  kind's limits, deadline + 30 s backstop, ttl 60 s), Secret owned by the Job.
  Stop deletes foreground and revokes. Tick ends a run whose Job failed or
  completed. The first config reconciles Jobs against live runs.
- **operator**: `app_services.coop` renders `<fleet>-pigeons` with default-deny
  and egress only to DNS, kernel and eventbus (by rendered app labels), a
  ResourceQuota, a Role for the coop's SA there, and a projected API token on
  the coop pod (operator 41116fe).
- **pigeon** (github.com/tacplus-dev/pigeon, registry 107): the generic host.
  Env from the Job (PIGEON_RUN_ID/THREAD_ID/TENANT/KIND/CALLER), identity from
  KERNEL_BOOTSTRAP_TOKEN_FILE, Ready then tasks then results on its thread,
  exits when the coop says ended. Built-in kind echo.ping.
- **thread auth**: PigeonMessage publish/subscribe admit
  `[^/]+/workloads/pigeon-[a-z0-9]+` beside "*" ("*" never admits a workload).
- Protocol: FAILURE_CODE_SCRIPT_FAILED added.

Open, and the identity question put to Jody (2026-10-25):
- **Who is the pigeon's parent.** Kernel's MintWorkload roots the child under
  the CALLER, with no on-behalf-of, and there is no S2S exchange that gives
  the coop a token with sub = dog (Grants are user delegations). Options:
  (1) kernel gains `parent_spiffe_id` on MintWorkload, honoured only for a
  configured workload broker (the coop) — recommended, matches A30 §3.2;
  (2) the dog mints and hands the bootstrap to the coop in CreatePigeon;
  (3) mint under the coop (system identity, no tenant) — what the runner does
  until (1) lands, behind the one Mint call.
- **A workload has no delegate.** Kernel resolves a subject's delegate by its
  full service path; a workload's path keeps `/workloads/<child>`
  (SubjectPath), so `findDelegateBySubject` misses and the pigeon is not S2S:
  it can mint no token for eventbus or kernel. Expected kernel change: a
  workload resolves to its parent service's delegate (the ancestor rule).
  To be confirmed on the fleet before coding.

Next:
- Slice 2: MODE_JOBS — client-go Job per pigeon, MintWorkload identity, the
  lease file (`lease_names` → FailedPrecondition until then), pigeons
  namespace RBAC. Then the generic pigeon host.
