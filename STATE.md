# State: coop

Overwritten to stay current (loopers and agents). Last updated: 2026-11-13 by the looper (#2).

- **Version / line:** `v0.1.0` (line `0.1`), a source tag on main (2026-11-07); its image and GitHub release are published by `.github/workflows/release.yml` (#2), which releases every `vX.Y.Z` tag from now on (README, Releases); tacplus floor 2.6, pinned to 2.6.1 in `tacplus.lock` (#8; stratum declares 2.6, so 2.5 cannot resolve `depends`)
- **Works:** `just gates` passes in the looper with its stock `tacplus` (tacplus 2.6.1, the `tacplus.lock` pin, in the `build:2.6` image, which has Go; #8): generation, tidy, gofmt, build, vet, test, the release scripts' tests. MODE_MOCK only (see README).
- **Released / deployed:** released as `ghcr.io/tacplus-dev/coop:0.1.0` and `:0.1`, linux/arm64 (amd64 off, as in the fleets: `PLATFORMS` in `release.yml`; #2). Deployed nowhere.
- **Records (#5):** `PigeonRun` is inserted into the dog's tenant's Stratum through its record write surface (`InsertIfAbsentPigeonRun`, `target_tenant` = the tid in the dog's SPIFFE subject, durability LOCAL), read back with `GetPigeonRun` when a retried transition finds its row there; nothing is published on the bus for Stratum. Not run against a live per-tenant Stratum.
- **Known gaps:** MODE_JOBS (Kubernetes Job per pigeon) not built.
- **Trace propagation (#7):** every handler (CreatePigeon, CancelPigeon, OnMessage, CoopConfig) copies its inbound `trace_id` onto the `PigeonMessage`s, `PigeonRun` records and kernel `WorkloadRevokeRequest`s it causes; `Tick` carries none (marked `// trace: unrelated`). Still dropped: the kernel-state calls behind `RunState` (Fetch, Put, All, so all of GetPigeon's) and `MintWorkload`, because the generated StateClient helpers take no `trace_id` and tacplus 2.6's `WorkloadMintRequest` has no field 200 (nor 2.5's).
- **Blocked on:** tacplus-dev/tacplus#313 for `trace_id` on the kernel-state calls and the mint (#7, open until then).
