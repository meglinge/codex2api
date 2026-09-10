//! WebSocket bridge: the gateway upgrades a loopback connection to `/ws`, the sender dials the
//! real upstream with the CLI's own stack and pipes frames both ways.
//!
//! Upstream dialling mirrors `codex-websocket-client` (`websocket-client/src/dialer.rs` in the
//! codex submodule) for the explicit-proxy and direct routes: the same forked
//! `tokio-tungstenite`, the same rustls configuration built by
//! `build_rustls_client_config_with_custom_ca` (native roots + Codex custom CA), the same
//! `permessage-deflate` extension config, and the same HTTP / HTTPS / SOCKS proxy tunnelling.
//! The copy exists only because that crate does not expose a connect-with-explicit-proxy
//! entry point; it should be replaced by one when the fork grows it.

use std::io;
use std::sync::Arc;

use axum::body::Body;
use axum::extract::Request;
use axum::extract::State;
use axum::http::HeaderMap;
use axum::http::HeaderName;
use axum::http::HeaderValue;
use axum::http::StatusCode;
use axum::http::header;
use axum::response::Response;
use futures::SinkExt;
use futures::StreamExt;
use hyper_util::rt::TokioIo;
use rustls::ClientConfig;
use rustls::pki_types::ServerName;
use tokio::io::AsyncRead;
use tokio::io::AsyncWrite;
use tokio::net::TcpStream;
use tokio_rustls::TlsConnector;
use tokio_tungstenite::Connector;
use tokio_tungstenite::WebSocketStream;
use tokio_tungstenite::client_async_tls_with_config;
use tokio_tungstenite::proxy::connect_via_proxy;
use tokio_tungstenite::tungstenite::Error as WsError;
use tokio_tungstenite::tungstenite::Message;
use tokio_tungstenite::tungstenite::client::IntoClientRequest;
use tokio_tungstenite::tungstenite::extensions::ExtensionsConfig;
use tokio_tungstenite::tungstenite::extensions::compression::deflate::DeflateConfig;
use tokio_tungstenite::tungstenite::handshake::derive_accept_key;
use tokio_tungstenite::tungstenite::protocol::Role;
use tokio_tungstenite::tungstenite::protocol::WebSocketConfig;
use tokio_tungstenite::tungstenite::proxy::ProxyConfig;
use tracing::debug;
use tracing::warn;

use crate::AppState;
use crate::CONTROL_METHOD;
use crate::CONTROL_PROXY;
use crate::CONTROL_TOKEN;
use crate::CONTROL_URL;
use crate::error_response;

/// Header order of a real Codex CLI WebSocket handshake, reconstructed from
/// `core/src/client.rs::build_websocket_headers`, `codex-api/src/endpoint/responses_websocket.rs`
/// (`merge_request_headers` appends the default headers, then `add_auth_headers`) and the
/// tungstenite client handshake, which writes its own fixed headers first and then the
/// request's header map in order.
pub(crate) const WS_CANONICAL_ORDER: &[&str] = &[
    "x-codex-beta-features",
    "originator",
    "x-client-request-id",
    "session-id",
    "thread-id",
    "x-codex-window-id",
    "x-codex-turn-metadata",
    "x-codex-parent-thread-id",
    "x-openai-subagent",
    "x-codex-routing-hint",
    "x-oai-attestation",
    "openai-beta",
    "x-responsesapi-include-timing-metrics",
    "user-agent",
    "x-openai-internal-codex-residency",
    "authorization",
    "chatgpt-account-id",
    "x-openai-fedramp",
];

/// Never forwarded upstream on the handshake: the loopback upgrade's own framing headers
/// (tungstenite regenerates them), hop-by-hop noise and the control headers.
const WS_DROPPED: &[&str] = &[
    "host",
    "connection",
    "upgrade",
    "sec-websocket-key",
    "sec-websocket-version",
    "sec-websocket-extensions",
    "sec-websocket-protocol",
    "sec-websocket-accept",
    "keep-alive",
    "proxy-connection",
    "transfer-encoding",
    "content-length",
    "accept-encoding",
    CONTROL_URL,
    CONTROL_METHOD,
    CONTROL_PROXY,
    CONTROL_TOKEN,
];

/// Same extension configuration as `codex-api/src/endpoint/responses_websocket.rs::websocket_config`.
fn websocket_config() -> WebSocketConfig {
    let mut extensions = ExtensionsConfig::default();
    extensions.permessage_deflate = Some(DeflateConfig::default());
    let mut config = WebSocketConfig::default();
    config.extensions = extensions;
    config
}

pub(crate) fn canonical_ws_headers(incoming: &HeaderMap) -> HeaderMap {
    let mut out = HeaderMap::with_capacity(incoming.len());
    let take = |out: &mut HeaderMap, name: &str| {
        if let Ok(header) = HeaderName::from_bytes(name.as_bytes()) {
            for value in incoming.get_all(&header) {
                out.append(header.clone(), value.clone());
            }
        }
    };
    for name in WS_CANONICAL_ORDER {
        take(&mut out, name);
    }
    let known: std::collections::HashSet<&str> = WS_CANONICAL_ORDER
        .iter()
        .chain(WS_DROPPED.iter())
        .copied()
        .collect();
    let mut seen = std::collections::HashSet::new();
    for (name, _) in incoming.iter() {
        let lower = name.as_str();
        if known.contains(lower) || !seen.insert(lower.to_string()) {
            continue;
        }
        take(&mut out, lower);
    }
    out
}

trait AsyncIo: AsyncRead + AsyncWrite + Unpin + Send {}
impl<T: AsyncRead + AsyncWrite + Unpin + Send> AsyncIo for T {}

struct ProxyEndpoint {
    config: ProxyConfig,
    tls: bool,
}

impl ProxyEndpoint {
    fn parse(url: &str) -> Result<Self, WsError> {
        let mut parsed = url::Url::parse(url).map_err(|_| invalid_proxy())?;
        let tls = parsed.scheme() == "https";
        if tls {
            let port = parsed.port_or_known_default().ok_or_else(invalid_proxy)?;
            parsed.set_scheme("http").map_err(|_| invalid_proxy())?;
            parsed.set_port(Some(port)).map_err(|_| invalid_proxy())?;
        }
        let config = ProxyConfig::parse(parsed.as_str()).map_err(|_| invalid_proxy())?;
        Ok(Self { config, tls })
    }
}

fn invalid_proxy() -> WsError {
    WsError::Io(io::Error::new(
        io::ErrorKind::InvalidInput,
        "invalid proxy url",
    ))
}

fn ws_host_port(
    request: &tokio_tungstenite::tungstenite::handshake::client::Request,
) -> Result<(String, u16), WsError> {
    let uri = request.uri();
    let host = uri
        .host()
        .ok_or_else(|| WsError::Io(io::Error::new(io::ErrorKind::InvalidInput, "missing host")))?
        .to_string();
    let port = uri
        .port_u16()
        .or_else(|| match uri.scheme_str() {
            Some("ws") => Some(80),
            Some("wss") => Some(443),
            _ => None,
        })
        .ok_or_else(|| {
            WsError::Io(io::Error::new(
                io::ErrorKind::InvalidInput,
                "unsupported scheme",
            ))
        })?;
    Ok((host, port))
}

fn host_port(host: &str, port: u16) -> String {
    if host.contains(':') && !host.starts_with('[') {
        format!("[{host}]:{port}")
    } else {
        format!("{host}:{port}")
    }
}

type UpstreamStream = WebSocketStream<tokio_tungstenite::MaybeTlsStream<Box<dyn AsyncIo>>>;

/// Dials the upstream the way `codex-websocket-client` does for `OutboundProxyRoute::Direct`
/// and `OutboundProxyRoute::Proxy { url, no_proxy: None }`.
async fn dial_upstream(
    url: &str,
    headers: HeaderMap,
    proxy: &str,
    tls_config: Arc<ClientConfig>,
) -> Result<(UpstreamStream, http::Response<Option<Vec<u8>>>), WsError> {
    let mut request = url.into_client_request()?;
    request.headers_mut().extend(headers);
    let (host, port) = ws_host_port(&request)?;

    let stream: Box<dyn AsyncIo> = if proxy.is_empty() {
        Box::new(
            TcpStream::connect(host_port(&host, port))
                .await
                .map_err(WsError::Io)?,
        )
    } else {
        let endpoint = ProxyEndpoint::parse(proxy)?;
        let tcp = TcpStream::connect(endpoint.config.authority())
            .await
            .map_err(WsError::Io)?;
        let tunnel: Box<dyn AsyncIo> = if endpoint.tls {
            let server_name = ServerName::try_from(endpoint.config.host.clone()).map_err(|_| {
                WsError::Io(io::Error::new(
                    io::ErrorKind::InvalidInput,
                    "bad proxy host",
                ))
            })?;
            Box::new(
                TlsConnector::from(Arc::clone(&tls_config))
                    .connect(server_name, tcp)
                    .await
                    .map_err(WsError::Io)?,
            )
        } else {
            Box::new(tcp)
        };
        connect_via_proxy(tunnel, &endpoint.config, &host, port).await?
    };

    client_async_tls_with_config(
        request,
        stream,
        Some(websocket_config()),
        Some(Connector::Rustls(tls_config)),
    )
    .await
}

fn is_ws_upgrade(headers: &HeaderMap) -> bool {
    let connection_upgrade = headers
        .get(header::CONNECTION)
        .and_then(|v| v.to_str().ok())
        .map(|v| {
            v.to_ascii_lowercase()
                .split(',')
                .any(|t| t.trim() == "upgrade")
        })
        .unwrap_or(false);
    let upgrade_ws = headers
        .get(header::UPGRADE)
        .and_then(|v| v.to_str().ok())
        .map(|v| v.eq_ignore_ascii_case("websocket"))
        .unwrap_or(false);
    connection_upgrade && upgrade_ws
}

/// GET /ws: validate the loopback upgrade, dial upstream first so its handshake response headers
/// can be echoed on our 101, then bridge frames until either side closes.
pub(crate) async fn ws_bridge(State(state): State<Arc<AppState>>, mut req: Request) -> Response {
    let headers = req.headers().clone();
    if !state.token.is_empty() {
        let presented = headers
            .get(CONTROL_TOKEN)
            .and_then(|v| v.to_str().ok())
            .unwrap_or("");
        if presented != state.token {
            return error_response(StatusCode::UNAUTHORIZED, "bad x-c2a-token");
        }
    }
    if !is_ws_upgrade(&headers) {
        return error_response(StatusCode::BAD_REQUEST, "expected websocket upgrade");
    }
    let Some(key) = headers.get(header::SEC_WEBSOCKET_KEY).cloned() else {
        return error_response(StatusCode::BAD_REQUEST, "missing sec-websocket-key");
    };
    let Some(url) = headers
        .get(CONTROL_URL)
        .and_then(|v| v.to_str().ok())
        .map(str::trim)
        .map(str::to_string)
    else {
        return error_response(StatusCode::BAD_REQUEST, "missing x-c2a-url");
    };
    if !url.starts_with("wss://") && !url.starts_with("ws://") {
        return error_response(StatusCode::BAD_REQUEST, "x-c2a-url must be ws:// or wss://");
    }
    let proxy = headers
        .get(CONTROL_PROXY)
        .and_then(|v| v.to_str().ok())
        .unwrap_or("")
        .trim()
        .to_string();
    let tls_config = match state.ws_tls_config() {
        Ok(config) => config,
        Err(err) => return error_response(StatusCode::BAD_GATEWAY, format!("tls: {err}")),
    };

    let upstream_headers = canonical_ws_headers(&headers);
    let (upstream, upstream_response) =
        match dial_upstream(&url, upstream_headers, &proxy, tls_config).await {
            Ok(pair) => pair,
            Err(WsError::Http(response)) => {
                // Upstream refused the handshake: surface its status/headers/body unchanged so the
                // gateway's bad-handshake diagnostics keep working.
                let mut out =
                    Response::new(Body::from(response.body().clone().unwrap_or_default()));
                *out.status_mut() = response.status();
                for (name, value) in response.headers() {
                    if matches!(
                        name.as_str(),
                        "connection" | "transfer-encoding" | "content-length"
                    ) {
                        continue;
                    }
                    out.headers_mut().append(name.clone(), value.clone());
                }
                return out;
            }
            Err(err) => {
                warn!(error = %err, "upstream websocket dial failed");
                return error_response(
                    StatusCode::BAD_GATEWAY,
                    format!("upstream websocket: {err}"),
                );
            }
        };

    let on_upgrade = hyper::upgrade::on(&mut req);
    tokio::spawn(async move {
        let io = match on_upgrade.await {
            Ok(io) => io,
            Err(err) => {
                warn!(error = %err, "loopback websocket upgrade failed");
                return;
            }
        };
        let downstream = WebSocketStream::from_raw_socket(
            TokioIo::new(io),
            Role::Server,
            // 回环这一跳不协商 permessage-deflate（101 里没有回给 Go 该扩展），
            // 用默认配置即明文帧；上游侧仍按真实客户端启用压缩。
            Some(WebSocketConfig::default()),
        )
        .await;
        bridge(downstream, upstream).await;
    });

    let mut response = Response::new(Body::empty());
    *response.status_mut() = StatusCode::SWITCHING_PROTOCOLS;
    let out = response.headers_mut();
    out.insert(header::CONNECTION, HeaderValue::from_static("upgrade"));
    out.insert(header::UPGRADE, HeaderValue::from_static("websocket"));
    out.insert(
        header::SEC_WEBSOCKET_ACCEPT,
        HeaderValue::from_str(&derive_accept_key(key.as_bytes()))
            .unwrap_or_else(|_| HeaderValue::from_static("")),
    );
    // Echo the upstream handshake response headers (x-codex-* usage windows, turn-state, model
    // hints) so the gateway reads them exactly as it did when it dialled upstream itself.
    for (name, value) in upstream_response.headers() {
        if matches!(
            name.as_str(),
            "connection"
                | "upgrade"
                | "sec-websocket-accept"
                | "sec-websocket-extensions"
                | "transfer-encoding"
                | "content-length"
        ) {
            continue;
        }
        out.append(name.clone(), value.clone());
    }
    response
}

async fn bridge<D, U>(downstream: WebSocketStream<D>, upstream: U)
where
    D: AsyncRead + AsyncWrite + Unpin + Send + 'static,
    U: futures::Stream<Item = Result<Message, WsError>>
        + futures::Sink<Message, Error = WsError>
        + Unpin
        + Send
        + 'static,
{
    let (mut down_tx, mut down_rx) = downstream.split();
    let (mut up_tx, mut up_rx) = upstream.split();

    let down_to_up = async {
        while let Some(frame) = down_rx.next().await {
            match frame {
                Ok(Message::Close(frame)) => {
                    let _ = up_tx.send(Message::Close(frame)).await;
                    break;
                }
                Ok(message) => {
                    if up_tx.send(message).await.is_err() {
                        break;
                    }
                }
                Err(err) => {
                    debug!(error = %err, "downstream websocket read ended");
                    let _ = up_tx.send(Message::Close(None)).await;
                    break;
                }
            }
        }
    };
    let up_to_down = async {
        while let Some(frame) = up_rx.next().await {
            match frame {
                Ok(Message::Close(frame)) => {
                    let _ = down_tx.send(Message::Close(frame)).await;
                    break;
                }
                Ok(message) => {
                    if down_tx.send(message).await.is_err() {
                        break;
                    }
                }
                Err(err) => {
                    debug!(error = %err, "upstream websocket read ended");
                    let _ = down_tx.send(Message::Close(None)).await;
                    break;
                }
            }
        }
    };
    tokio::select! {
        _ = down_to_up => {}
        _ = up_to_down => {}
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn canonical_ws_headers_follow_cli_order_and_drop_upgrade_noise() {
        let mut incoming = HeaderMap::new();
        for (name, value) in [
            ("host", "127.0.0.1:8799"),
            ("connection", "Upgrade"),
            ("upgrade", "websocket"),
            ("sec-websocket-key", "abc"),
            ("sec-websocket-version", "13"),
            ("authorization", "Bearer t"),
            (
                "user-agent",
                "codex-tui/0.153.4 (Ubuntu 22.4.0; x86_64) unknown (codex-tui; 0.153.4)",
            ),
            ("openai-beta", "responses_websockets=2026-02-06"),
            ("x-c2a-url", "wss://chatgpt.com/backend-api/codex/responses"),
            ("session-id", "s"),
            ("x-codex-beta-features", "remote_compaction_v2"),
            ("chatgpt-account-id", "acct"),
            ("x-custom-ops", "1"),
        ] {
            incoming.append(
                HeaderName::from_static(name),
                HeaderValue::from_static(value),
            );
        }
        let out = canonical_ws_headers(&incoming);
        let names: Vec<&str> = out.keys().map(|k| k.as_str()).collect();
        assert_eq!(
            names,
            vec![
                "x-codex-beta-features",
                "session-id",
                "openai-beta",
                "user-agent",
                "authorization",
                "chatgpt-account-id",
                "x-custom-ops",
            ]
        );
    }
}
