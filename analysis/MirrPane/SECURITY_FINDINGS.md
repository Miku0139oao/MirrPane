# MirrPane — Security Hardening Report

- **System**: MirrPane (`legacy/MirrPane` → `D:\new\airplay-go`, junction)
- **Date**: 2026-11-22
- **Method**: 5 class-scoped finders (injection, auth/session, secrets, dependency CVEs, input validation) → de-duplication → one adversarial refuter per distinct finding → second judge for every Critical/High survivor. 34 agents, read-only.
- **Tooling**: `govulncheck` v1.9.0 (Go 1.26.3, DB 2026-11-11), OSV API (Go + Pub ecosystems), `gitleaks --redact`, `go vet ./...`, manual sink review. No npm/pip manifests exist; Dart/Flutter SDK absent locally, so pubspec.lock was queried against OSV directly.

## Scorecard

| Severity | Count |
|---|---|
| Critical | 0 |
| High | 4 |
| Medium | 7 |
| Low | 3 |
| **Confirmed** | **14** |
| Refuted by adversarial verification | 11 of 25 judged (false-positive rate **44%**) |

Top categories: CWE-306 (auth missing, 2), CWE-312 (cleartext sensitive data, 2), CWE-117 (log injection, 2), CWE-1395/937 (vulnerable dependencies/toolchain, 2); remaining are CWE-129, 321, 352, 426, 295, 532.

Instruction-shaped content aimed at automated analysis: **none found** (0 injection flags).

## Coverage gaps

All 5 finder classes returned and every finding was judged.

## Findings

| ID | Sev | CWE | Location | Finding | One-line exploit |
|---|---|---|---|---|---|
| SEC-001 | High | CWE-306 | `backend/internal/receiver/receiver.go:345` | Unauthenticated LAN peer can preempt and hijack the active mirror/audio session | Complete the unauthenticated `/fp-setup` + `/setup` exchange, then connect to the advertised mirror port; `serve()` closes the legitimate sender and publishes the attacker's frames. |
| SEC-002 | High | CWE-532 | `backend/internal/receiver/receiver.go:382` | Per-session AES stream keys written in plaintext to the always-on log | Read `logs/airplayd.log` (another local user, support bundle, copied portable folder) and recover the FairPlay-derived stream key. |
| SEC-003 | High | CWE-1395 | `backend/go.mod:3` | Go toolchain 1.26.3 (manifest 1.26.0) is outdated: 20 reachable stdlib CVEs | Unauthenticated XML-plist TEARDOWN reaches the unpatched `encoding/xml` decoder (GO-2026-6088) and crashes the receiver; unpatched `crypto/x509` (GO-2026-6809) accepts a revoked intermediate on the FFmpeg update download. |
| SEC-004 | High | CWE-129 | `backend/internal/mirror/server.go:629` | Unvalidated slice offset in `splitHEVC` lets a network peer panic the receiver | Send a short type-0x01 packet whose payload ends in `hvc1`; `payload[0x75:]` panics with slice-bounds-out-of-range and no `recover` exists, killing `airplayd`. |
| SEC-005 | Medium | CWE-117 | `backend/internal/receiver/receiver.go:430` | Log injection/forging via unescaped plist stream values in RTSP `/setup` | A `ct`/`spf` string containing `\n` forges attacker-controlled log lines to obscure an attack. |
| SEC-006 | Medium | CWE-426 | `backend/internal/ffmpeg/runtime.go:85` | FFmpeg DLLs may be resolved from PATH / CWD-derived directories | A planted `ffmpeg.dll`/dependency earlier in the search path is loaded as native code. |
| SEC-007 | Medium | CWE-352 | `backend/cmd/airplayd/state.go:209` | Local UI API lacks authentication, Origin and Host validation | A webpage or DNS-rebinding origin issues cross-origin requests to `127.0.0.1:<api>` (e.g. `/stop`, volume). |
| SEC-008 | Medium | CWE-321 | `backend/internal/config/config.go:147` | Pairing private key derived deterministically from non-secret machine identifiers | MachineGuid + broadcast device ID let a local attacker re-derive the receiver's pairing identity. |
| SEC-009 | Medium | CWE-312 | `backend/internal/config/config.go:171` | Pairing private key persisted in cleartext in a portable file | Anyone who can read `config/config.json` (0644) obtains the Ed25519 seed. |
| SEC-010 | Medium | CWE-295 | `backend/internal/ffmpeg/runtime.go:139` | GO-2026-6809: `crypto/x509` trusts revoked intermediates during FFmpeg update download | A revoked intermediate on the TLS path to the FFmpeg download could be accepted; fixed by the toolchain bump in SEC-003. |
| SEC-011 | Medium | CWE-937 | `backend/go.mod:3` | Build uses a Go toolchain with 20 reachable stdlib vulnerabilities | Same root cause as SEC-003; fixed by the same patch. |
| SEC-012 | Low | CWE-117 | `backend/internal/ipc/rtsp.go:338` | Log/terminal escape injection via raw RTSP method and target | Crafted method/target strings inject terminal escapes or forged lines into the console log. |
| SEC-013 | Low | CWE-306 | `backend/internal/ipc/server.go:278` | LAN-exposed `/stop` and TEARDOWN control actions require no authentication | A LAN peer can stop the receiver or tear down a session. |
| SEC-014 | Low | CWE-312 | `backend/config/config.json:5` | Pairing private-key seed stored in cleartext and duplicated into distribution staging folders | A copied/archived staging folder exposes the live receiver identity. **Credential finding.** |

> 1 hardcoded credential found — inventory in `SECRETS.local.md` (gitignored; not for sharing).

## Dependency / toolchain CVEs

| Component | Installed | Advisory | Fixed in |
|---|---|---|---|
| Go stdlib (`net/http`, `net/textproto`, `net/url`, `encoding/xml`, `encoding/asn1`, `crypto/tls`, `crypto/x509`) | go1.26.3 (manifest 1.26.0) | 20 reachable: GO-2026-6859, 6617, 6613, 6612, 6611, 6610, 6608, 6607, 6605, 6603, 5026, 6090, 6089, 6088, 5972, 5856, 5039, 5037, 6218, 6809 | Go 1.26.6 – 1.26.11 (all fixed by 1.26.11) |
| `golang.org/x/crypto` | v0.58.0 (module graph only, not compiled) | GO-2026-5932 (unsafe openpgp) | v0.60.0 |
| `golang.org/x/net` | v0.62.0 | none open (already above the 0.60.0 fixes) | — |
| FFmpeg runtime (pinned BtbN build) | N-129166-gae9f4540cb-20261121, SHA256-pinned and verified before install | none known at audit date | — |
| Pub packages (22 in `pubspec.lock`) | — | no advisories | — |

8 further stdlib advisories are present in the build but not reachable from compiled code (govulncheck "not called").

## Refuted findings (11)

Dropped after adversarial verification: hard-coded-credential claims about `config.json` as a *shipped* secret (the release zips exclude `config/` and the file is gitignored; downgraded to SEC-014, Low); deterministic-key-derivation claims rated Medium/high where the control boundary is MachineGuid, not the file (kept as SEC-008, Medium); unauthenticated XML TEARDOWN DoS rated as a standalone defect (it is a stdlib advisory covered by SEC-003); `record.go` plaintext recording (debug feature, same-user); `golang.org/x/crypto` advisory (not imported); "world-readable config" (file is gitignored and local-only); duplicated staging seeds rated High (impact Low, folded into SEC-014); and three duplicate/overstated variants of the above.

## Remediation Log

Patches are written for every Critical and High finding except SEC-001 (see below — its draft was rejected in review and is recorded as needs-manual-remediation). No Critical findings were reported. All hunks live in `security_remediation.patch`; there are no credential hunks (SEC-014 is Low and its fix is rotation + ACLs, not a diff).

| ID | Fix | Patch location |
|---|---|---|
| SEC-001 | **Needs manual remediation.** A drafted patch (503 on `/setup` while a stream is active; reject a second mirror TCP connection) was rejected by the independent patch review: it does not authenticate the sender, breaks legitimate reconnect/zombie-socket recovery, can be held open by an unauthenticated peer (lockout DoS), and leaves the shared audio key reconfiguration path untouched. No SEC-001 hunk is shipped. | — |
| SEC-002 | Stop logging candidate key bytes; log only the candidate name | `security_remediation.patch` (receiver.go) |
| SEC-003 | Raise `backend/go.mod` to `go 1.26.11`; CI resolves this via `go-version-file` | `security_remediation.patch` (backend/go.mod) |
| SEC-004 | Bound-check the `0x75` parameter-set offset before slicing; add a short-payload regression test | `security_remediation.patch` (mirror/server.go, mirror/server_test.go) |
| SEC-005–SEC-013 | Not drafted (severity below the Critical/High threshold); recommendations are in the Findings table | — |
| SEC-014 | Rotate the receiver identity (delete `config.json` and staging copies, re-pair senders) and restrict ACLs to the current user; no code diff drafted | `SECRETS.local.md` |

SEC-001 residual risk (what a proper fix needs, per review): authenticate `/setup` (pair-verify/PIN or allowlist) before `Prepare` or `audioRx.Configure`; bind the mirror data connection to the pending `/setup` (expected source IP + stream ID, short window); track stream liveness (last valid frame) and reap idle/half-open connections instead of first-connect-wins; make audio key state per-sender; drop the mirror client and reset keys on full TEARDOWN and on control-connection close.

Verification performed before review: the patch applies with `patch -p1` and the patched tree builds (`CGO_ENABLED=0 go build ./...`) and passes `go test ./internal/mirror ./internal/receiver` on a clean `git archive` copy.

## Patch Review

Independent security-auditor review on a clean `git archive HEAD` copy.

**Round 1** (full patch, including the SEC-001 draft):

| Hunk | Cites | Verdict | Reason |
|---|---|---|---|
| receiver.go key log | SEC-002 | RESOLVES | Removes the only plaintext logging of the derived AES key. |
| receiver.go 503 on `/setup` | SEC-001 | PARTIAL | No authentication; the ekey block already reconfigured audio keys before refusing, and it breaks legitimate reconnect. |
| mirror Prepare returns false | SEC-001 | PARTIAL | Refuses while any TCP peer is connected, not while an authenticated live stream exists; TOCTOU with the real sender. |
| mirror serve rejects newcomer | SEC-001 | INTRODUCES-RISK | First-connect-wins lets an unauthenticated peer hold the mirror port and lock out the real sender; no idle reaping. |
| splitHEVC bounds check | SEC-004 | RESOLVES | `len(payload) < 0x75+5` guards the slice; downstream offsets stay bounded. |
| short-payload regression test | SEC-004 | RESOLVES | Test passes and exercises the original panic. |
| go.mod 1.26.11 | SEC-003 | RESOLVES | `govulncheck` drops from 20 reachable stdlib CVEs to zero; CI resolves the directive. |

**Round 2** (revised patch after removing the rejected SEC-001 hunks): all remaining hunks RESOLVES; SEC-001 recorded as needs manual remediation above. The reviewer confirmed no raw credential appears in the shareable patch and that the patched tree builds and passes `go test ./internal/mirror ./internal/receiver`. The finding comments were then moved above each file header so both `patch -p1` and `git apply --check` accept the patch (both verified).

**Round 2 operational note (SEC-003)**: `go.mod` declares a minimum (`go 1.26.11`), not a pinned download. Under the default `GOTOOLCHAIN=auto` and in CI (`go-version-file: backend/go.mod`, both workflows) the patched toolchain is selected automatically; a host with `GOTOOLCHAIN=local` and an older Go fails closed with a clear version error rather than silently building the vulnerable stdlib.
