# scrcpy server (pinned)

`scrcpy-server-v5.0.1.jar` is the unmodified Android server from
[Genymobile/scrcpy v5.0.1](https://github.com/Genymobile/scrcpy/releases/tag/v5.0.1),
redistributed under the Apache License 2.0 (see `LICENSE.txt`).

- Source: `scrcpy-server-v5.0.1` release asset
- SHA-256: `764eb6f79811d5211fe9df341120882ba9994c7a61b897d7bf3fb662e53bc536`
- The Go code embeds this file (`//go:embed`) and pushes it to
  `/data/local/tmp/scrcpy-server.jar` on the device, exactly as scrcpy's own
  client does.

To update, download the new release asset, verify it against that release's
`SHA256SUMS.txt`, replace the jar, and bump `ServerVersion` in the scrcpy
package. The wire protocol is version-coupled: never mix a jar and a client
version.
