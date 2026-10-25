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

Learned from the first real pigeons on seatrials (2026-10-25, in order):
- The coop's Role grants secrets create/get/delete, not update: the Secret
  is now created after the Job, born with its ownerReference.
- The tick checked a run still in REQUESTED (CreatePigeon between recording
  and starting) and read the missing Job as gone. REQUESTED is left alone
  until the ready timeout.
- The Secret's files were root's 0400; the pigeon runs as 65532. fsGroup
  65532 and mode 0440.
- From the pigeons namespace the fleet's bare names (kernel-0.kernel,
  eventbus0, as the bootstrap config and ServiceHealthState carry them)
  resolve to nothing: the pod gets `<fleet ns>.svc.cluster.local` as a DNS
  search domain, so nothing else changes.
- **The workload's bootstrap exchange works with no delegate.** Kernel took
  the minted bootstrap token, issued the pigeon's tokens and its TLS cert
  (X509Mint) for `/system/services/coop/workloads/pigeon-<run>`. So the
  "a workload has no delegate" worry below is narrower than written: it
  bites, if at all, on service-audience tokens (eventbus) — to be seen.
- Kernel refused the pigeon's read of `pigeon.PigeonConfigState.config`
  (a workload is not the service "pigeon"), and it is right: a pigeon has
  no config root; its parameters are the Job's env. tacplus did not model
  that shape — a program with only event bindings got no service manager
  at all ("a CLI tool"). tacplus now treats an actor whose own proto
  declares event_publish/event_watch as a WORKLOAD: health state injected,
  service manager with publishers and subscriptions, handler = health +
  shutdown (registry-and-floor, after f65ffc2). pigeon.proto's `Thread`
  message carries only the bindings.

- A pigeon Job must pull its image Always: the fleet names images by a
  mutable tag and the kubelet kept a stale pigeon.
- The chassis advertises os.Hostname() as its health endpoint and kernel
  requires an IP or FQDN; a Job pod's name is neither, and an FQDN hostname
  (hostname + subdomain + setHostnameAsFQDN) came out longer than a hostname
  may be. tacplus's Go service manager now honours SERVICE_ADVERTISE_HOST
  when listening on a wildcard; the Job sets it from status.podIP.
  (Go only; Java/Rust get the same variable when they host workloads.)
- With that, the pigeon reaches SERVING. Its eventbus publish and
  subscribe then failed with "bootstrap response missing refresh_token": a
  workload's first exchange turns the one-shot bootstrap token into a
  refresh token; later renewals present it and kernel answers a
  service-typed refresh with an access token alone, while the Go minter
  demanded a new refresh token every time. tacplus now keeps the held
  refresh token (registry-and-floor after e33ad50). So the "no delegate"
  worry was not the failure mode; whether kernel authorises an eventbus
  audience for the workload is the next thing the run will show.

**MODE_JOBS proven on seatrials, 2026-10-25 00:5xZ** (fleet-seatrials with
tacplus fcb665b, coop 49e12b1, pigeon 4ed1acd): `seatrials pingpigeon` as
alpha and as beta each returned its own message echoed by a real pigeon
(`pigeon_spiffe_id` = `/system/services/coop/workloads/pigeon-<run>`), the
Job and its Secret gone within seconds of the dog's cancel; a coop restart
in the middle of a run left the run alive (the new coop reconciled it and
kept its Job) and the result still arrived; the pigeons namespace's default
SA can only read API discovery, and the pod mounts no token at all. Not yet
tested live: deadline expiry (needs a dog that does not cancel) and the
ready-timeout path.

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
