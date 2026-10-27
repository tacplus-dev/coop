# Roadmap: coop

Owned by Jody; changed only by an agent at Jody's request. Loopers propose in PROPOSALS.md.

## Now
The fleet's one door to pigeons (short-lived sandboxed workers): `Coop.CreatePigeon`/`CancelPigeon`/`GetPigeon` and the per-run thread on `coop.PigeonMessageBus.stream`.
Go. Unreleased; tacplus floor 2.4 (declared, #1 still open).

## Next
- Nothing approved and unblocked; every open approved issue is in Later.

## Later
- Declare the tacplus floor 2.4 and pass the gates on it (#1), blocked on tacplus-dev/tacplus#189
- Release v0.1.0 and publish its image (#2), blocked on tacplus-dev/looper#12 and #1
- Base image alpine:3.22 to alpine:3.24 (#3), blocked on #1

## Not doing
- A Rust rewrite: Rust is the tacplus-dev default and coop should have been Rust, but Jody accepts Go here for now
