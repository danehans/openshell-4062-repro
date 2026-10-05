# OpenShell #4062 reproduction (Go SDK)

Minimal reproduction for [NVIDIA/OpenShell#4062](https://github.com/NVIDIA/OpenShell/issues/4062):
a committed per-sandbox policy revision does not always advance the sandbox
resource version, so a stale `ExpectedResourceVersion` is still accepted and
silently overwrites a newer policy.

Uses only the public Go SDK (`github.com/NVIDIA/OpenShell/sdk/go`, pinned in `go.mod`)
against a running Gateway. No database or internal API access.

## What it does

1. Creates one disposable sandbox with an inline policy (no providers, no annotations),
   waits for Ready, stops it, and waits for Stopped plus a stable resource version.
   Stopping avoids supervisor status writes that could advance the version and mask the bug.
2. Reads resource version `R` and policy `P`.
3. Builds policy A (`P` + rule `writer-a`) and policy B (`P` + rule `writer-b`).
4. `Config().Update` with A, `ExpectedResourceVersion = R`; rereads the sandbox version.
5. `Config().Update` with B, **the same stale `R`**.
6. Reads the final policy and revision list. On the fixed path it also rereads, merges B into A, and retries.
7. Deletes the sandbox.

Sequential calls are sufficient: they model two clients that read the same snapshot.

## Run

```sh
go run . --address localhost:8080 --tls-dir /path/to/certs --image <sandbox-image>
```

`--tls-dir` holds `ca.crt`, `tls.crt`, `tls.key` (omit for plaintext). `--image` is optional.
Exit code: `0` expected behavior, `2` bug reproduced, `1` anything else.

## Observed

| | Bug (unpatched) | Expected (fixed) |
|---|---|---|
| Sandbox resource version after A | unchanged (`R`) | greater than `R` |
| B with stale `R` | accepted | rejected, gRPC `Aborted` |
| Latest policy | `writer-b` only; A lost | `writer-a` only until retry |
| Revisions inserted by B | yes | no |

### Unpatched Gateway

Gateway image `ghcr.io/nvidia/openshell/gateway:374c035962dada63852ec1dea073fea618ed76c5`
(upstream `main` at that commit), SQLite backend (`--db-url sqlite:...`), SDK
`v0.0.0-20260930202246-912a077bd641`:

```
snapshot:  resource_version=27 policy_version=2
writer A:  accepted, policy_version=3, sandbox resource_version=27 (was 27)
writer B:  ACCEPTED with stale resource_version=27, policy_version=4
final:     policy_version=4 revisions=4 writer-a=false writer-b=true
RESULT: BUG REPRODUCED - stale write accepted; writer A's policy change was lost
```

### Gateway with the proposed fix (local build)

```
snapshot:  resource_version=28 policy_version=2
writer A:  accepted, policy_version=3, sandbox resource_version=29 (was 28)
writer B:  rejected: Conflict: persist policy revision failed due to concurrent modification (current resource_version: 29)
           grpc_code=Aborted
final:     policy_version=3 revisions=3 writer-a=true writer-b=false
retry:     accepted after reread; writer-a=true writer-b=true
RESULT: EXPECTED BEHAVIOR - stale write rejected with Aborted; fresh merge accepted
```

## Notes

- Annotations are omitted on purpose: changing them advances the version independently.
- A and B differ in content on purpose: identical content can take an idempotent path with no new revision.
- The check uses the exact gRPC code (`Aborted`) rather than `v1.IsConflict`, which also matches `FailedPrecondition`.
