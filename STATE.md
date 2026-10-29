# State: coop

Overwritten to stay current (loopers and agents). Last updated: 2026-10-29 by the looper (#6).

- **Version / line:** unreleased (no vX.Y.Z tag); tacplus floor 2.5 (#6; stratum declares 2.5, so 2.4 cannot resolve `depends`)
- **Works:** `just gates` passes with the tacplus 2.5.1 binary (`TACPLUS_BIN`) and a Go toolchain on PATH: generation, tidy, gofmt, build, vet, test. MODE_MOCK only (see README).
- **Released / deployed:** nowhere.
- **Known gaps:** MODE_JOBS (Kubernetes Job per pigeon) not built; records still reach Stratum through the event bus (#5).
- **Blocked on:** tacplus-dev/tacplus#213 for the gates in the looper: its `tacplus` runs the `tacplus/tacplus:2.5` image, which has no `go` for goimports (tacplus#188); the build image with Go (tacplus#193) ships in v2.6.0.
