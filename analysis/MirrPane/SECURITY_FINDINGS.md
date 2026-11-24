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

The historical standalone patch covers every Critical and High finding except SEC-001, whose original draft was rejected in review. SEC-001 now RESOLVES under the owner-approval model: unknown verified senders remain blocked until the local user explicitly approves their fingerprint. No Critical findings were reported. Historical patch hunks live in `security_remediation.patch`; there are no credential hunks (SEC-014 is Low and its fix is rotation + ACLs, not a diff).

| ID | Fix | Patch location |
|---|---|---|
| SEC-001 | **RESOLVES under the owner-approval model (rounds 5/6).** Only explicit local approval persists a pending verified fingerprint. The last control close immediately invalidates pairing and derived control keys while media has a bounded grace; fresh verification is required for setup. Displayed approval requests are pinned and new requests are source-limited. Same-IP sharing of a still-live verified control session remains an explicit residual. | source tree (`trust`, `pairing`, `receiver`, `mirror`, `ipc`, `cmd/airplayd`); no standalone patch hunk |
| SEC-002 | Stop logging candidate key bytes; log only the candidate name | `security_remediation.patch` (receiver.go) |
| SEC-003 | Raise `backend/go.mod` to `go 1.26.11`; CI resolves this via `go-version-file` | `security_remediation.patch` (backend/go.mod) |
| SEC-004 | Bound-check the `0x75` parameter-set offset before slicing; add a short-payload regression test | `security_remediation.patch` (mirror/server.go, mirror/server_test.go) |
| SEC-005–SEC-013 | Implemented in the source tree in the second hardening round (below); see the round-2 verdicts | tree changes, no separate patch file |
| SEC-014 | Rotate the receiver identity (delete `config.json` and staging copies, re-pair senders) and restrict ACLs to the current user; no code diff drafted | `SECRETS.local.md` |

SEC-001 original review requirements: authenticate `/setup` before `Prepare` or `audioRx.Configure`; bind the data connection to a short-lived setup and stream ID; track valid-frame liveness instead of first-connect-wins; protect per-sender audio keys; clean up media and keys on full teardown/disconnect. These are implemented, with control-close grace and address-family tolerance as detailed below.

SEC-001 implementation (2026-11-22, amended in round 6): pair-verify and setup never auto-enroll an identity. Unverified media setup is 403 `ErrNotPaired`; a verified but untrusted sender gets 403 `ErrUntrusted` and cannot change media keys. Only explicit local approval persists trust. On the owner's last control close, the receiver immediately replaces pairing/FairPlay state and clears ECDH, AES candidates, IV and stream ID; mirror/audio retain their independent media keys only for the 5-second grace. Generic control requests and `/fp-setup` cannot cancel that grace. Pair-setup, pair-verify and setup requests can resume control handling, but setup still needs fresh verified proof. The approval queue remains 8 entries with a 10-minute first-seen TTL, admits at most 2 new entries per IPv4 address or IPv6 /64 within a rolling TTL, and protects the request most recently displayed by `/api/state` from churn eviction. Admission history is retained after approval/rejection/eviction and bounded to 128 active sources; untracked sources are refused at that bound until expiry/reset. Rejected records remain bounded to 32. Trust reset clears all session/media, pending/rejected, source-budget and pin state without rotating receiver identity.

SEC-001 residual under owner approval: the local user/UI or an equivalent local API process is trusted to approve the intended fingerprint; labels are untrusted hints and no PIN is provided. Same-IP peers can still share a verified session while at least one control connection for that IP remains live, including auxiliary connections; this round fixes inheritance after the last close, not per-connection cryptographic binding. Legacy pairing endpoints remain open, and CTR/CBC NAL/AAC framing has no cryptographic message authentication; UDP injection/replay remains outside this fix. Actual pairing/setup requests can cancel media grace even if later validation fails, but generic/FP requests cannot, and canceled grace no longer preserves old proof. Multi-source floods, shared IPv6 /64 quotas, or source-history saturation can temporarily delay new approval requests; already displayed requests stay actionable until expiry/owner decision. SEC-203/204/205/206/207 and MP-05 remain follow-ups and are not claimed resolved here. One sidecar owns its data directory; live-iPhone approval/retry/fresh-pairing/reconnect validation remains owed.

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

## Round 2 — remaining findings implemented in the tree (2026-11-22)

An independent security-auditor re-reviewed the full working-tree diff (55 KB, 20 files) after the round-1 patch was applied. Other rows retain those historical verdicts; SEC-001 shows the current round-5 owner-approval implementation status, with independent/live-device validation still pending.

| ID | Verdict | Change |
|---|---|---|
| SEC-001 | **RESOLVES under the owner-approval model (rounds 5/6)** | Verified-key checks and explicit local approval remain. Last-control-close proof reset prevents same-IP inheritance after disconnection; media grace survives independently. Source quotas and displayed-entry pinning keep pending approvals actionable. Shared audio ownership, short-lived stream-ID key binding and keyless-peer deadlines remain. Live-session same-IP sharing and unauthenticated legacy framing are explicit residuals. |
| SEC-005 | **RESOLVES** | `ct`/`spf` logged with `%q`. |
| SEC-006 | **RESOLVES** | `Find()` no longer consults PATH/CWD; only override, exe-adjacent dir, verified cache. Dependent DLL lookup already restricted by `LoadLibraryEx`. CI exports `MIRRPANE_FFMPEG_DIR` for tests. |
| SEC-007 | **RESOLVES** | Host + Origin validation; `Origin: null` rejected after review (it is the sandboxed/data: CSRF vector); `Vary: Origin` added. |
| SEC-008 | **RESOLVES** | New identities use `crypto/rand`; `PairingID` is now a random per-install UUID so two copies on one machine cannot share an identity with different keys. |
| SEC-009 | **PARTIAL (Windows ACL)** | Atomic temp+rename and 0600 mode; on Windows `os.Chmod` does not restrict other local users — a DPAPI/DACL follow-up remains. |
| SEC-012 | **RESOLVES** | RTSP method/target/protocol logged with `%q`. |
| SEC-013 | **RESOLVES** | `/stop` requires a loopback peer; LAN gets 403, `/info` stays LAN-accessible. |

Verification: full `CGO_ENABLED=0` build/vet/test and `-race` on receiver, mirror, pairing, audio, ipc, config, ffmpeg pass under Go 1.26.11.

Allowlist/MP-06/MP-07 follow-up verification: Go 1.26.11, `CGO_ENABLED=0` build/vet and `go test -count=1 ./...` pass with `MIRRPANE_FFMPEG_DIR=third_party/ffmpeg/bin` (absolute path). `CGO_ENABLED=1`, `CC=D:\DevTools\mingw64\bin\gcc.exe` and `go test -race -count=1` pass for receiver, mirror, pairing, config, trust, ipc and cmd/airplayd. New tests use memory connections, injected timers and temporary files; no live device/network is needed.

### Tracked follow-ups (non-blocking, from the review)

- **Live-device validation of SEC-001**: reconnect after network change, screen lock/unlock, audio-only and video-only setups, dual-stack address families.
- **Windows ACL for `config.json`** (SEC-009 / MP-04): the working-tree diff includes protected DACL helpers, save/load integration and effective Windows ACL tests for config and trust files. This is coordinator-owned work; unsupported-filesystem behavior and deployment validation remain separate review items. This is not a missing-code claim (see SEC-209 correction below).
- **Per-session locking** (MP-05): move crypto work off the receiver-wide mutex and rate-limit pairing per source to reduce a LAN CPU/DoS vector.
- **Address-family tolerance** (MP-06): **implemented, capacity handling corrected in round 4 (SEC-102)**. A fresh setup permits an alternate IPv4/IPv6 data address when no healthy stream owns the slot; same-address reconnects retain their fast path. Tentative peers cannot publish media or claim ownership before a valid decrypted frame within setup expiry. At 16 attempts, admission evicts the oldest foreign attempt, protecting the setup source when another candidate exists; alternate-family newcomers are not rejected merely because the map is full. Socket-free tests cover a full IPv6-control/IPv4-data map and rejection of valid-key frames after expiry.
- **Control-close grace** (MP-07): **implemented, proof reset and generic-hook removal in round 6 (SEC-201)**. Last close immediately clears pairing, ECDH and FairPlay-derived control state while a 5 s media timer remains. Only pair-setup/pair-verify/setup handling can cancel that timer; OPTIONS, `/info` and `/fp-setup` cannot. Generic cancellation hooks/counters were removed. Obsolete timer generations are ignored; explicit teardown/disconnect/stop remain immediate. Reconnect tests repeat pairing and key setup within grace; live-device migration remains owed.

## Round 5/6 — owner approval and review follow-ups (2026-11-22)

| ID | Status | Current result |
|---|---|---|
| SEC-001 / SEC-101 | **RESOLVES under the owner-approval model** | Neither pairing nor setup grants trust. Verified unknown setup creates only a bounded pending request and returns 403. The local approve endpoint persists the pending fingerprint; only a later setup from that approved identity can configure media. Reject provides a bounded cooldown; reset clears approved/pending/rejected identities and media without rotating the receiver key. |
| MP-06 / SEC-102 | **Capacity/expiry fixes implemented** | Attempts record setup, creation time and source IP. Full-map admission replaces the oldest foreign attempt instead of rejecting a legitimate alternate-family data address; same-address replacement and the 16 cap remain. Valid-key frames cannot commit after setup expiry. |
| SEC-107 | **Generic-cancellation path removed in round 6** | Generic request hooks no longer resume receiver sessions and `/fp-setup` uses non-resuming lookup. Only actual pairing/setup handlers can cancel grace; these still do not authenticate every individual control request. |
| SEC-201 | **Fixed: reset on last control close** | Pairing/FairPlay sessions are replaced and ECDH/AES candidates/IV/stream ID are cleared immediately, independently of media grace. Same-IP FairPlay plus attacker-key setup is refused; a trusted identity can reconnect only after fresh verification and key setup. |
| SEC-202 | **Fixed: source quotas and displayed-entry pin** | At most 2 new pending admissions per IPv4 address or IPv6 /64 during the rolling 10-minute TTL. Queue capacity remains 8; the oldest displayed request is protected from churn until expiry or its own approval/rejection (or explicit full reset). Source history is bounded and retained after pending removal. |
| SEC-208 | **Fixed: connecting state cleanup** | Last non-owner control close returns to idle when no active/connecting session remains. Unowned FairPlay/pairing probes cannot overwrite a live owner's UI state; callbacks execute without holding the receiver lock. |
| SEC-209 | **Scope correction** | The full diff does include config protected-DACL code/tests and Dart `pendingSender` parsing, approve/reject/reset HTTP methods and the local approval UI. These are coordinator-owned changes, not absent implementations; this round did not edit frontend or config ACL files. |
| SEC-203 / SEC-204 / SEC-205 / SEC-206 / SEC-207 | **Follow-ups retained** | Track the reviewer's individual remaining findings separately; this session/approval fix does not claim them resolved. |

Regression coverage includes last-close pairing/key reset, same-IP attacker refusal, fresh trusted reconnect, non-resuming FairPlay traffic, unauthorized-close idle cleanup, 20-key churn from one of two sources, IPv4-mapped and IPv6 /64 quotas, source-budget retention/expiry, full-queue churn with a displayed pin, owner decision/expiry/reset pin release and bounded source metadata. Existing owner-approval, media-admission, API, persistence and timer-race tests remain. IPC hook removal touches only the generic callback declaration/dispatch, preserving the coordinator's rate-limiter/logging changes.

Round-4 verification: Go 1.26.11 with `CGO_ENABLED=0` and the pinned `D:\new\airplay-go\third_party\ffmpeg\bin` runtime passes `go build ./...`, `go vet ./...` and `go test -count=1 ./...`. With `CGO_ENABLED=1` and `CC=D:\DevTools\mingw64\bin\gcc.exe`, `go test -race -count=1 ./internal/receiver ./internal/mirror ./internal/pairing ./cmd/airplayd` passes. No live-device validation, commit or push was performed.

### Owner-approval API contract for the Dart client

All API requests require a loopback TCP peer and valid Host/Origin. Existing `/api/state` keys are unchanged; `pendingSender` is always present and is either `null` or the oldest unexpired request, for example `{"fingerprint":"sha256:<64 lowercase hex digits>","remote":"192.168.1.20","ageSeconds":12}`. Returning it pins that request against churn eviction until expiry or its owner's approval/rejection/reset. `ageSeconds` is a nonnegative integer; optional `label` is a sanitized, sender-supplied hint of at most 64 runes.

| Endpoint | Request | Result |
|---|---|---|
| `GET /api/state` | No body | Existing state plus `pendingSender` object or `null` |
| `POST /api/pairing/trust/approve` | `{"fingerprint":"sha256:..."}` | `{"ok":true}`; 400 for bad/missing fingerprint, 404 for unknown/expired/already-consumed pending request, 500 for persistence/unavailable receiver |
| `POST /api/pairing/trust/reject` | Same fingerprint body | `{"ok":true}`; idempotent dismissal and 10-minute suppression; 400 for invalid input |
| `POST /api/pairing/trust/reset` | No body required | `{"ok":true}`; clears trusted, pending and rejected records and pairing/media state; 500 on persistence failure |

Decision/reset routes require POST (otherwise 405); non-loopback or invalid Host/Origin is 403. Approval/rejection JSON is limited to 1024 bytes. The sender may retry setup after approval; if the iPhone already abandoned its 403 setup, the user may need to re-select Screen Mirroring once. After the last control close, retrying requires fresh pair-setup/pair-verify and stream key setup. The diff includes Dart client/UI wiring added by the coordinator; this round keeps its API shapes unchanged.

Round-5 verification: Go 1.26.11 with `CGO_ENABLED=0` and `MIRRPANE_FFMPEG_DIR=D:\new\airplay-go\third_party\ffmpeg\bin` passes `go build ./...`, `go vet ./...` and `go test -count=1 ./...`. With `CGO_ENABLED=1` and `CC=D:\DevTools\mingw64\bin\gcc.exe`, `go test -race -count=1 ./internal/receiver ./internal/trust ./cmd/airplayd` passes. Tests use in-memory requests/connections, temporary trust files and an injected approval clock; no live-iPhone validation, commit or push was performed.

Round-6 verification: Go 1.26.11 with `CGO_ENABLED=0` and the pinned `D:\new\airplay-go\third_party\ffmpeg\bin` runtime passes `go build ./...`, `go vet ./...` and `go test -count=1 ./...`. With `CGO_ENABLED=1`, `CC=D:\DevTools\mingw64\bin\gcc.exe`, `go test -race -count=1 ./internal/receiver ./internal/mirror ./internal/ipc ./cmd/airplayd` passes (IPC added to the requested race set because its generic hook was removed). New scenarios use in-memory connections, temporary trust files and injected timers/clocks. This round did not edit `ratelimit*.go`, server logging or frontend files, and performed no live-iPhone validation, commit or push.

## Round 7 — final hardening pass (2026-11-22)

Independent re-review of the owner-approval build. Addressed:

| Item | Status | Change |
|---|---|---|
| SEC-201 | RESOLVES (scenario fixed) | Last control connection close now clears `pair`/`ecdh`/FairPlay key state while keeping the 5 s media grace; FairPlay cannot renew grace; a same-IP peer must complete a fresh pair-verify and owner approval. **Residual (SEC-213):** a peer that shares the source IP of a *currently connected, verified* owner can still piggyback that session; per-connection proof binding is a follow-up. |
| SEC-202 | RESOLVES | Max 2 pending requests per source (IPv4 or IPv6 /64) per TTL; the entry shown by `/api/state` is pinned from churn. |
| SEC-203 | RESOLVES | Rate-limiter tokens are per address (one IPv6 host cannot drain its /64 neighbours); bucket creation capped at 32 per /64 window. |
| SEC-204 / SEC-210 | RESOLVES | 429 logging sampled to one line/second; every logged RTSP target is truncated to 120 bytes; `set_parameter` logs its length, never its body. |
| SEC-211 | RESOLVES | The prefix-window map is bounded (oldest eviction at 4096). |
| SEC-212 | RESOLVES | Multi-source churn no longer refuses new sources: the least-recently-admitted source budget is evicted. |
| SEC-208 | RESOLVES | Status returns to idle when unauthorized connections close; probes do not overwrite a live stream's UI state. |
| SEC-209 | RESOLVES | This document now matches the shipped tree, including the config ACL and Dart approval UI. |

Remaining, explicitly not fixed (tracked): SEC-213 (same-IP live-session piggyback; needs per-control-connection proof), SEC-214 (narrow `connecting` state race), SEC-205/206 (data-directory ACL and non-ACL volumes), media framing has no cryptographic integrity, and live iPhone validation of the approval flow (approve → reselect → setup; reconnect; screen lock; dual-stack) is still owed. SEC-014: the local staging seed was rotated and key-bearing logs scrubbed during this work.

## Follow-up — Per-connection control binding & state race (SEC-213 / SEC-214)

| Item | Status | Change |
|---|---|---|
| SEC-213 | **RESOLVES** | `ipc.Server` now keys each accepted TCP control connection by its distinct connection identity (`ip:port`, or synthetic `#N` suffix when portless/colliding) and fires `OnDisconnect(connKey)` per connection close while keeping `ratelimit.go` keyed by peer IP. `Receiver` isolates `clientSession` proof per `connKey` (bounded at 64 sessions, preserving `activeConn`), strips the IP for `mirror.Prepare` and pending-approval quotas, blocks same-IP unverified/non-owner `SETUP` and `TEARDOWN`, and allows live/grace handoff only after fresh `pair-verify` proves the same trusted `activeFingerprint`. |
| SEC-214 | **RESOLVES** | Receiver UI state transitions are recorded under `r.mu` and drained serially under `r.stateMu` (never holding `r.mu` during `OnState`), preventing stale `StatusConnecting` emissions from overwriting newer `StatusIdle` or `StatusLive` states. |
