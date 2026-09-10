# c2a-sender

Loopback HTTP forwarder that performs codex2api's upstream requests with the **same
reqwest / hyper / TLS stack the Codex CLI links**, so the gateway inherits the CLI's
transport fingerprint instead of Go's.

## Why

Go's `net/http` (and the uTLS Chrome profile) produce a TLS ClientHello, HTTP/2
`SETTINGS` / `WINDOW_UPDATE` behaviour and header emission order that no real Codex
client ever produces. The CLI is `reqwest` + `hyper` with the platform TLS backend
(OpenSSL on Linux, Schannel on Windows, SecureTransport on macOS); rustls is only used
through the custom-CA path. This binary builds its client the way
`codex_login::default_client` does — `reqwest::Client::builder()` from the Codex
workspace, the shared ChatGPT Cloudflare cookie store,
`build_reqwest_client_with_custom_ca` — so, running on Linux, its wire behaviour matches
a real Codex CLI on Linux byte for byte at the transport layer.

The Codex sources come from the git submodule at `../third_party/codex` (the
`meglinge/codex` fork, branch `codexs`). `Cargo.lock` is copied from
`third_party/codex/codex-rs/Cargo.lock` so every shared dependency resolves to the exact
version the CLI is built with.

## Build

```sh
git submodule update --init third_party/codex
cd sender
cargo build --release            # -> target/release/c2a-sender
```

The pinned toolchain (`rust-toolchain.toml`, copied from the submodule) is picked up by
rustup automatically.

## Run

```sh
./target/release/c2a-sender --listen 127.0.0.1:8799 --token "$C2A_SENDER_TOKEN"
```

| flag / env | default | meaning |
| --- | --- | --- |
| `--listen` / `C2A_SENDER_LISTEN` | `127.0.0.1:8799` | loopback address only; non-loopback addresses are rejected |
| `--token` / `C2A_SENDER_TOKEN` | empty | shared secret required in `x-c2a-token`; empty disables the check |
| `--client-idle-secs` / `C2A_SENDER_CLIENT_IDLE_SECS` | `600` | per-(pool, proxy) reqwest clients are dropped after this idle time |
| `--log-filter` / `C2A_SENDER_LOG` | `info` | tracing filter (`RUST_LOG` overrides) |

`CODEX_CA_CERTIFICATE` / `SSL_CERT_FILE` are honoured exactly as in the CLI.

Then point codex2api at it:

```sh
CODEX_TRANSPORT_MODE=rust \
CODEX_RUST_SENDER_URL=http://127.0.0.1:8799 \
CODEX_RUST_SENDER_TOKEN="$C2A_SENDER_TOKEN" \
CODEX_UPSTREAM_TRANSPORT=http \
./codex2api
```

## Protocol

```
POST /forward
  x-c2a-url:    https://chatgpt.com/backend-api/codex/responses   (required, absolute)
  x-c2a-method: GET | POST | ...                                  (default POST)
  x-c2a-proxy:  http:// | socks5:// | socks5h:// URL              (optional; empty = direct)
  x-c2a-token:  shared secret                                      (when configured)
  x-c2a-pool:   isolation pool, e.g. acct-51                       (optional; empty = default pool)
  <every other header is sent upstream, re-ordered into the CLI's canonical order>
  <body sent upstream verbatim>
→ upstream status, upstream headers, upstream body streamed as-is
502 + x-c2a-error on a transport failure
GET /healthz → 200 ok
```

Hop-by-hop headers, `accept-encoding` and the `x-c2a-*` control headers never reach
upstream — `x-c2a-pool` included, so the pool name stays on the loopback leg. Header order is rebuilt from the CLI sources (`core/src/client.rs`,
`codex-api/src/endpoint/responses.rs`, `http-client/src/request.rs`,
`codex-api/src/auth.rs`) with `user-agent` trailing, as reqwest appends default headers
after request headers.

## Isolation pools

A real CLI is one process per account, so its connection pool, its Cloudflare cookie jar and
its TLS session tickets all belong to that one account. This binary serves a whole pool, so
that separation has to be reconstructed: `x-c2a-pool` names the isolation pool and everything
cached per caller is keyed by `(pool, proxy)` — the `reqwest` client (hence its connections),
a **per-pool** Cloudflare cookie jar, and the rustls `ClientConfig` used by the WebSocket path
(rustls keeps its session-resumption cache inside the config).

Without it, one account's `__cf_bm` would be presented on behalf of every other account and the
edge could resume account A's TLS session for account B — two strong cross-account correlators.
codex2api sets the header to `acct-<id>`; an empty or absent value means the default pool, which
is what single-tenant use and the gateway's maintenance requests get.

## Scope

- Covers every HTTP request codex2api makes through `getPooledClient` /
  `getMaintenanceClient` (Responses, compact, usage, models).
- Covers the WebSocket upstream too (`GET /ws`): the gateway upgrades a loopback connection,
  the sender dials upstream with the forked `tokio-tungstenite` + rustls (native roots +
  Codex custom CA) and `permessage-deflate`, echoes the upstream handshake response headers on
  its 101, and pipes frames both ways. The loopback leg itself is uncompressed.
- WebSocket TLS is rustls on every platform in the real CLI, so that path matches Windows,
  macOS and Linux personas alike. HTTP TLS matches a real CLI **on the same OS as this
  binary** (native TLS); a Linux sender paired with a Windows or macOS persona still differs
  there.
