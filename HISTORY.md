# History: coop

Append-only: add entries at the end, never edit or remove one (CI enforces it). One line each:
`- <YYYY-MM-DD> <who: looper|agent|jody> <kind: issue|decision|release|deploy> <one line> (<links>)`

- 2026-10-28 looper issue golang.org/x/net v0.57.0 has 5 reachable HTTP/2 vulnerabilities (GO-2026-6603/6610/6611/6612/6617): bump to v0.62.0 (#4, ae1732e)
- 2026-11-06 looper issue Declare the tacplus floor: 2.4 (#1)
- 2026-11-06 looper issue tacplus floor 2.5 is behind the latest released line (2.6): move the floor to 2.6 and commit tacplus.lock (#8, e4fe5d1)
- 2026-11-06 looper issue Base image: bump alpine:3.22 to the latest line, alpine:3.24 (#3)
- 2026-11-06 looper issue Write records to Stratum through its write surface: Stratum no longer ingests them from the event bus (#5, de2177d)
- 2026-11-07 looper issue Gates fail on main: stratum needs tacplus 2.5; move the floor from 2.4 to 2.5 (#6)
- 2026-11-15 looper release v0.1.1 (https://github.com/tacplus-dev/coop/releases/tag/v0.1.1)
- 2026-11-15 looper issue gosec G703 (HIGH, CWE-22) at cmd/coop/main.go:130: os.ReadFile(os.Getenv("BOOTSTRAP_CONFIG")) fails the build's security checks (#12)
- 2026-11-15 looper release v0.1.2 (https://github.com/tacplus-dev/coop/releases/tag/v0.1.2)
- 2026-11-15 looper issue STATE.md Blocked on names closed tacplus-dev/tacplus#313: name tacplus-dev/tacplus#327 (traced helpers, 2.7) instead (#10, f111dc3)
- 2026-11-15 looper issue STATE.md Blocked on still names the closed tacplus-dev/tacplus#313 (as history): the Blocked-on check reads it as a blocker; name only tacplus-dev/tacplus#327 and #7 (#13, fdea07d)
- 2026-11-15 looper release v0.1.3 (https://github.com/tacplus-dev/coop/releases/tag/v0.1.3)
- 2026-11-15 looper issue GO-2026-6859 (net/http, stdlib) reachable: the pinned build:2.6.1 compiles with go1.27.3; move tacplus.lock to 2.6.8 (go1.27.4) (#9, b4bbe34)
- 2026-11-16 looper issue Release: tag vMAJOR.MINOR.PATCH (start at v0.1.0) and publish its image (#2)
- 2026-11-16 looper issue semgrep github-actions-mutable-action-tag (MEDIUM, 6 new): release.yml:54,94,99,110,111,112 use actions by tag, not SHA (#2) (#11)
- 2026-11-17 looper issue Security checks: new non-blocking trivy DS-0026 (LOW), the Dockerfile has no HEALTHCHECK: fix or except (#15)
- 2026-11-17 looper issue Release image runs launcher c54ae30, before launcher#7's fix of its TLS health probe skipping certificate verification (gosec G402, HIGH) (#14)
- 2026-11-18 looper issue trivy DS-0001 (MEDIUM, new) at Dockerfile:24: FROM launcher-${TARGETARCH} names a stage through an ARG, which trivy reads as an untagged image (#16)
- 2026-11-23 looper release v0.1.6 (https://github.com/tacplus-dev/coop/releases/tag/v0.1.6)
- 2026-11-23 looper issue STATE.md: Blocked on names closed tacplus-dev/tacplus#327; coop now waits on the 2.7.0 release (tacplus-dev/tacplus#954) (#19)
- 2026-11-23 looper issue Pinned images behind their lines: relock (tacplus.lock: tacplus 2.6.13 → 2.6.14, and 1 more) (#18)
