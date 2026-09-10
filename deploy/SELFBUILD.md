# 自建镜像部署（不依赖 GitHub Actions / ghcr.io）

在目标机器上直接从 git 仓库构建 codex2api 与 c2a-sender 两个镜像并用 docker compose 运行。

## 前置条件

- Docker 24+ 与 Docker Compose v2（`docker compose version`）
- 首次构建需要联网拉取 Go / Node / Rust 基础镜像与依赖；Rust 发送器会编译 codex 子模块里的
  `http-client` 等 crate，8 核以上机器约 2–5 分钟

## 步骤

```sh
git clone --recurse-submodules https://github.com/meglinge/codex2api.git
cd codex2api
cp .env.sqlite.example .env
# 至少设置：
#   C2A_SENDER_TOKEN=$(openssl rand -hex 24)    # codex2api 与发送器之间的共享密钥
#   ADMIN_SECRET=...                            # 可选；不设则首次访问 /admin 走页面初始化
docker compose -f docker-compose.selfbuild.yml up -d --build
docker compose -f docker-compose.selfbuild.yml logs -f --tail=100
```

默认只绑定 `127.0.0.1:8080`；对外暴露请在 `.env` 里设置 `BIND_HOST=0.0.0.0` 或放在反向代理后面。

## 更新

```sh
git pull --recurse-submodules
docker compose -f docker-compose.selfbuild.yml up -d --build
```

## 组成

| 服务 | 镜像 | 说明 |
| --- | --- | --- |
| `codex2api` | `codex2api:selfbuild`（`Dockerfile`） | Go 网关 + 前端，SQLite + 内存缓存 |
| `sender` | `codex2api-sender:selfbuild`（`Dockerfile.sender`） | Rust 发送器，与 codex2api 共享网络命名空间，只监听 `127.0.0.1:8799` |

codex2api 以 `CODEX_TRANSPORT_MODE=rust` 运行，HTTP 与 WebSocket 上游流量全部经发送器发出；
`CODEX_UPSTREAM_TRANSPORT` 默认 `ws`。发送器的实现与协议见 `sender/README.md`。
