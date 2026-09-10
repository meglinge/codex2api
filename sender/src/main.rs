//! c2a-sender: a loopback forwarder that performs upstream HTTP and WebSocket requests with
//! the same reqwest / hyper / tungstenite / TLS stack the Codex CLI links, so a gateway written
//! in another language (codex2api, Go) inherits the CLI's transport fingerprint: TLS
//! ClientHello, HTTP/2 SETTINGS / WINDOW_UPDATE / priority behaviour, hyper's header emission
//! and the WebSocket handshake shape.
//!
//! Protocol (all on 127.0.0.1):
//!
//! ```text
//! POST /forward
//!   x-c2a-url:    https://chatgpt.com/backend-api/codex/responses   (required)
//!   x-c2a-method: GET | POST | ...                                  (default POST)
//!   x-c2a-proxy:  socks5h://user:pass@host:1080                     (optional)
//!   x-c2a-token:  shared secret                                      (when configured)
//!   <every other header is sent upstream, re-ordered into the CLI's canonical order>
//!   <body is sent upstream verbatim>
//! → status, upstream headers, upstream body streamed as-is
//!
//! GET /ws  (WebSocket upgrade)
//!   x-c2a-url:    wss://chatgpt.com/backend-api/codex/responses    (required)
//!   x-c2a-proxy / x-c2a-token as above
//!   <every other header is sent on the upstream handshake, re-ordered like the CLI>
//! → 101 carrying the upstream handshake response headers, then frames piped both ways
//! ```
//!
//! On a transport failure the response is 502 with `x-c2a-error` carrying the reason.
//!
//! The HTTP client for a given proxy is built exactly like `codex_login::default_client`:
//! `reqwest::Client::builder()` from this workspace (native TLS by default, rustls only
//! through the custom-CA path), the shared ChatGPT Cloudflare cookie store, and
//! `build_reqwest_client_with_custom_ca`. TLS therefore matches a real CLI running on the
//! same operating system as this binary. The WebSocket path uses rustls through the forked
//! tungstenite, exactly as `codex-websocket-client` does on every platform.

mod ws;

use std::collections::HashMap;
use std::net::SocketAddr;
use std::sync::Arc;
use std::sync::OnceLock;
use std::time::Duration;

use anyhow::Context;
use axum::Router;
use axum::body::Body;
use axum::extract::State;
use axum::http::HeaderMap;
use axum::http::HeaderName;
use axum::http::HeaderValue;
use axum::http::StatusCode;
use axum::response::IntoResponse;
use axum::response::Response;
use axum::routing::get;
use axum::routing::post;
use bytes::Bytes;
use clap::Parser;
use codex_http_client::build_reqwest_client_with_custom_ca;
use codex_http_client::build_rustls_client_config_with_custom_ca;
use codex_http_client::with_chatgpt_cloudflare_cookie_store;
use codex_utils_rustls_provider::ensure_rustls_crypto_provider;
use rustls::ClientConfig;
use tokio::sync::Mutex;
use tracing::info;
use tracing::warn;

pub(crate) const CONTROL_URL: &str = "x-c2a-url";
pub(crate) const CONTROL_METHOD: &str = "x-c2a-method";
pub(crate) const CONTROL_PROXY: &str = "x-c2a-proxy";
pub(crate) const CONTROL_TOKEN: &str = "x-c2a-token";
const ERROR_HEADER: &str = "x-c2a-error";

/// Header order of a real Codex CLI `/responses` request, reconstructed from
/// `core/src/client.rs` (extra headers), `codex-api/src/endpoint/responses.rs`
/// (`x-client-request-id`, accept), `http-client/src/request.rs` (content-encoding,
/// content-type), `codex-api/src/auth.rs` (auth headers extended last) and reqwest's
/// default-header merge (`user-agent` appended after request headers). hyper writes HPACK
/// entries in map order, so the order is part of the wire fingerprint.
const CANONICAL_ORDER: &[&str] = &[
    "x-codex-installation-id",
    "x-codex-beta-features",
    "x-codex-turn-state",
    "originator",
    "x-codex-window-id",
    "x-codex-turn-metadata",
    "x-codex-parent-thread-id",
    "x-openai-subagent",
    "x-openai-memgen-request",
    "session-id",
    "thread-id",
    "x-oai-attestation",
    "x-codex-routing-hint",
    "x-openai-internal-codex-responses-lite",
    "x-client-request-id",
    "accept",
    "content-encoding",
    "content-type",
    "authorization",
    "chatgpt-account-id",
    "x-openai-fedramp",
];

/// Headers that always trail the request headers because reqwest / hyper add them last.
const TRAILING_ORDER: &[&str] = &["user-agent", "x-openai-internal-codex-residency"];

/// Never forwarded: hop-by-hop, loopback-transport artefacts, and control headers.
const DROPPED: &[&str] = &[
    "host",
    "connection",
    "keep-alive",
    "proxy-connection",
    "transfer-encoding",
    "te",
    "trailer",
    "upgrade",
    "content-length",
    "accept-encoding",
    CONTROL_URL,
    CONTROL_METHOD,
    CONTROL_PROXY,
    CONTROL_TOKEN,
];

#[derive(Parser, Debug)]
#[command(
    name = "c2a-sender",
    about = "Codex-fingerprint HTTP/WebSocket forwarder for codex2api"
)]
struct Args {
    /// Loopback address to listen on.
    #[arg(long, env = "C2A_SENDER_LISTEN", default_value = "127.0.0.1:8799")]
    listen: SocketAddr,
    /// Shared secret required in `x-c2a-token`; empty disables the check.
    #[arg(long, env = "C2A_SENDER_TOKEN", default_value = "")]
    token: String,
    /// Idle clients (per proxy) are dropped after this many seconds.
    #[arg(long, env = "C2A_SENDER_CLIENT_IDLE_SECS", default_value_t = 600)]
    client_idle_secs: u64,
    /// tracing filter (overridden by RUST_LOG).
    #[arg(long, env = "C2A_SENDER_LOG", default_value = "info")]
    log_filter: String,
}

struct ClientEntry {
    client: reqwest::Client,
    last_used: std::time::Instant,
}

pub(crate) struct AppState {
    pub(crate) token: String,
    idle: Duration,
    clients: Mutex<HashMap<String, ClientEntry>>,
    ws_tls: OnceLock<Result<Arc<ClientConfig>, String>>,
}

impl AppState {
    /// Returns the HTTP client for `proxy` (empty = direct), building it the way the CLI does.
    async fn client_for(&self, proxy: &str) -> anyhow::Result<reqwest::Client> {
        let key = proxy.trim().to_string();
        let mut clients = self.clients.lock().await;
        let now = std::time::Instant::now();
        clients.retain(|_, entry| now.duration_since(entry.last_used) < self.idle);
        if let Some(entry) = clients.get_mut(&key) {
            entry.last_used = now;
            return Ok(entry.client.clone());
        }
        let client = build_codex_like_client(&key)?;
        clients.insert(
            key,
            ClientEntry {
                client: client.clone(),
                last_used: now,
            },
        );
        Ok(client)
    }

    /// rustls configuration for the WebSocket path, built once like
    /// `WebSocketConnector::new` (`WebSocketTlsMode::ExplicitCodexTls`).
    pub(crate) fn ws_tls_config(&self) -> Result<Arc<ClientConfig>, String> {
        self.ws_tls
            .get_or_init(|| {
                build_rustls_client_config_with_custom_ca().map_err(|err| err.to_string())
            })
            .clone()
    }
}

/// Mirrors `HttpClientBuilder::base_reqwest_builder` + `build_with_custom_ca_fallback` for the
/// default Codex client, with one difference: the proxy is explicit instead of coming from the
/// process environment, so every account can use its own egress.
fn build_codex_like_client(proxy: &str) -> anyhow::Result<reqwest::Client> {
    let mut builder = reqwest::Client::builder();
    builder = with_chatgpt_cloudflare_cookie_store(builder);
    builder = if proxy.is_empty() {
        builder.no_proxy()
    } else {
        builder.proxy(
            reqwest::Proxy::all(proxy).with_context(|| format!("invalid proxy url {proxy}"))?,
        )
    };
    build_reqwest_client_with_custom_ca(builder).map_err(|err| anyhow::anyhow!("{err}"))
}

/// Re-orders the headers received from the gateway into the CLI's canonical wire order and
/// drops everything that must not cross the loopback boundary.
fn canonical_headers(incoming: &HeaderMap) -> reqwest::header::HeaderMap {
    let mut out = reqwest::header::HeaderMap::with_capacity(incoming.len());
    let mut take = |name: &str| {
        if let Ok(header) = HeaderName::from_bytes(name.as_bytes()) {
            for value in incoming.get_all(&header) {
                out.append(header.clone(), value.clone());
            }
        }
    };
    for name in CANONICAL_ORDER {
        take(name);
    }
    let known: std::collections::HashSet<&str> = CANONICAL_ORDER
        .iter()
        .chain(TRAILING_ORDER.iter())
        .chain(DROPPED.iter())
        .copied()
        .collect();
    let mut seen = std::collections::HashSet::new();
    for (name, _) in incoming.iter() {
        let lower = name.as_str();
        if known.contains(lower) || !seen.insert(lower.to_string()) {
            continue;
        }
        take(lower);
    }
    for name in TRAILING_ORDER {
        take(name);
    }
    out
}

pub(crate) fn error_response(status: StatusCode, message: impl Into<String>) -> Response {
    let message: String = message.into();
    let value = HeaderValue::from_str(&message.replace(['\r', '\n'], " "))
        .unwrap_or_else(|_| HeaderValue::from_static("forward failed"));
    let mut response = (status, message).into_response();
    response.headers_mut().insert(ERROR_HEADER, value);
    response
}

async fn forward(State(state): State<Arc<AppState>>, headers: HeaderMap, body: Bytes) -> Response {
    if !state.token.is_empty() {
        let presented = headers
            .get(CONTROL_TOKEN)
            .and_then(|v| v.to_str().ok())
            .unwrap_or("");
        if presented != state.token {
            return error_response(StatusCode::UNAUTHORIZED, "bad x-c2a-token");
        }
    }
    let Some(url) = headers
        .get(CONTROL_URL)
        .and_then(|v| v.to_str().ok())
        .map(str::trim)
    else {
        return error_response(StatusCode::BAD_REQUEST, "missing x-c2a-url");
    };
    if !url.starts_with("https://") && !url.starts_with("http://") {
        return error_response(StatusCode::BAD_REQUEST, "x-c2a-url must be absolute");
    }
    let proxy = headers
        .get(CONTROL_PROXY)
        .and_then(|v| v.to_str().ok())
        .unwrap_or("")
        .trim()
        .to_string();
    let client = match state.client_for(&proxy).await {
        Ok(client) => client,
        Err(err) => return error_response(StatusCode::BAD_GATEWAY, format!("client: {err:#}")),
    };

    let method = match headers.get(CONTROL_METHOD).and_then(|v| v.to_str().ok()) {
        None | Some("") => reqwest::Method::POST,
        Some(raw) => match raw.trim().to_ascii_uppercase().parse::<reqwest::Method>() {
            Ok(method) => method,
            Err(_) => return error_response(StatusCode::BAD_REQUEST, "bad x-c2a-method"),
        },
    };
    let upstream_headers = canonical_headers(&headers);
    let mut request = client.request(method, url).headers(upstream_headers);
    if !body.is_empty() {
        request = request.body(body);
    }
    let upstream = match request.send().await {
        Ok(response) => response,
        Err(err) => {
            warn!(error = %err, "upstream request failed");
            return error_response(StatusCode::BAD_GATEWAY, format!("upstream: {err}"));
        }
    };

    let status = upstream.status();
    let mut response_headers = HeaderMap::new();
    for (name, value) in upstream.headers() {
        let lower = name.as_str();
        if matches!(
            lower,
            "connection" | "transfer-encoding" | "keep-alive" | "content-length"
        ) {
            continue;
        }
        response_headers.append(name.clone(), value.clone());
    }
    let stream = futures::TryStreamExt::map_err(upstream.bytes_stream(), std::io::Error::other);
    let mut response = Response::new(Body::from_stream(stream));
    *response.status_mut() = status;
    *response.headers_mut() = response_headers;
    response
}

async fn healthz() -> &'static str {
    "ok"
}

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    let args = Args::parse();
    let filter = tracing_subscriber::EnvFilter::try_from_default_env()
        .unwrap_or_else(|_| tracing_subscriber::EnvFilter::new(args.log_filter.clone()));
    let _ = tracing_subscriber::fmt().with_env_filter(filter).try_init();
    ensure_rustls_crypto_provider();

    if !args.listen.ip().is_loopback() {
        anyhow::bail!(
            "c2a-sender must listen on a loopback address (got {})",
            args.listen
        );
    }
    let state = Arc::new(AppState {
        token: args.token.clone(),
        idle: Duration::from_secs(args.client_idle_secs.max(1)),
        clients: Mutex::new(HashMap::new()),
        ws_tls: OnceLock::new(),
    });
    let app = Router::new()
        .route("/forward", post(forward))
        .route("/ws", get(ws::ws_bridge))
        .route("/healthz", get(healthz))
        .with_state(state);
    let listener = tokio::net::TcpListener::bind(args.listen)
        .await
        .with_context(|| format!("bind {}", args.listen))?;
    info!(listen = %args.listen, "c2a-sender ready");
    axum::serve(listener, app)
        .with_graceful_shutdown(async {
            let _ = tokio::signal::ctrl_c().await;
        })
        .await
        .context("serve")?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn canonical_headers_follow_cli_order_and_drop_transport_noise() {
        let mut incoming = HeaderMap::new();
        for (name, value) in [
            (
                "user-agent",
                "codex-tui/0.153.4 (Ubuntu 22.4.0; x86_64) unknown (codex-tui; 0.153.4)",
            ),
            ("authorization", "Bearer t"),
            ("accept", "text/event-stream"),
            ("x-codex-turn-metadata", "{}"),
            ("content-type", "application/json"),
            ("host", "127.0.0.1:8799"),
            ("accept-encoding", "gzip"),
            (
                "x-c2a-url",
                "https://chatgpt.com/backend-api/codex/responses",
            ),
            ("originator", "codex-tui"),
            ("session-id", "s"),
            ("x-custom-ops", "1"),
            ("chatgpt-account-id", "acct"),
            ("content-encoding", "zstd"),
        ] {
            incoming.append(
                HeaderName::from_static(name),
                HeaderValue::from_static(value),
            );
        }
        let out = canonical_headers(&incoming);
        let names: Vec<&str> = out.keys().map(|k| k.as_str()).collect();
        assert_eq!(
            names,
            vec![
                "originator",
                "x-codex-turn-metadata",
                "session-id",
                "accept",
                "content-encoding",
                "content-type",
                "authorization",
                "chatgpt-account-id",
                "x-custom-ops",
                "user-agent",
            ]
        );
    }
}
