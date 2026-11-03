# State: coop

Overwritten to stay current (loopers and agents). Last updated: 2026-11-03 by the looper (#7).

- **Version / line:** unreleased (no vX.Y.Z tag); tacplus floor 2.5 (#6; stratum declares 2.5, so 2.4 cannot resolve `depends`)
- **Works:** `just gates` passes with the tacplus 2.5.1 binary (`TACPLUS_BIN`) and a Go toolchain on PATH: generation, tidy, gofmt, build, vet, test. MODE_MOCK only (see README).
- **Released / deployed:** nowhere.
- **Known gaps:** MODE_JOBS (Kubernetes Job per pigeon) not built; records still reach Stratum through the event bus (#5).
- **Trace propagation (#7):** every handler (CreatePigeon, CancelPigeon, OnMessage, CoopConfig) copies its inbound `trace_id` onto the `PigeonMessage`s, `PigeonRun` records and kernel `WorkloadRevokeRequest`s it causes; `Tick` carries none (marked `// trace: unrelated`). Still dropped: the kernel-state calls behind `RunState` (Fetch, Put, All, so all of GetPigeon's) and `MintWorkload`, because the generated StateClient helpers take no `trace_id` and tacplus 2.5's `WorkloadMintRequest` has no field 200.
- **Blocked on:** tacplus-dev/tacplus#313 for `trace_id` on the kernel-state calls and the mint (#7, open until then).
- **Also blocked on:** tacplus-dev/tacplus#213 for the gates in the looper: its `tacplus` runs the `tacplus/tacplus:2.5` image, which has no `go` for goimports (tacplus#188); the build image with Go (tacplus#193) ships in v2.6.0.
