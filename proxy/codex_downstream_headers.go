package proxy

import (
	"os"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ==================== 下行元数据事件的上游头清洗 ====================
//
// WebSocket 传输路径把上游握手响应头整块塞进 codex.response.metadata 事件的 headers
// 对象里下发。HTTP 传输路径不转发任何一个上游响应头——下游看到的响应头全部由网关
// 自己构造（Content-Type / Cache-Control / Retry-After / X-Accel-Buffering），
// x-codex-* 用量头在 parseCodexUsageHeaders 里消费完就丢弃，上游 x-request-id 与
// cf-ray 从不出现在下游。
//
// 于是同一批上游头在两条链路上待遇不同：HTTP 那条治理得很干净，WS 这条整块放行，
// 还额外带出 set-cookie 与 x-codex-plan-type。下游据此可以读到账号套餐、上游请求 id
// 与 Cloudflare 节点标识——这正是「一个值有几条协议路径就要治理几次」的典型症状。
//
// 这里按白名单裁剪 headers 对象，让两条链路给下游同一幅图像。只保留
// x-codex-turn-state：它已由 rewriteCodexTurnStateInEvent 换成网关签发的 token，
// 是下游续链真正需要的句柄，不是上游身份。
//
// CODEX_METADATA_HEADER_PASSTHROUGH=1 回到旧的整块透传行为（仅用于排障对照）。

const codexMetadataHeaderPassthroughEnv = "CODEX_METADATA_HEADER_PASSTHROUGH"

// codexDownstreamMetadataHeaderAllowlist 是元数据事件里允许到达下游的头（小写）。
// 新增条目前先确认它在 HTTP 链路上也到得了下游，否则两条链路又会分叉。
var codexDownstreamMetadataHeaderAllowlist = map[string]struct{}{
	strings.ToLower(codexTurnStateHeader): {},
}

func codexMetadataHeaderPassthroughEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(codexMetadataHeaderPassthroughEnv))) {
	case "1", "true", "yes", "on", "enabled":
		return true
	default:
		return false
	}
}

// sjsonEscapePathKey 转义 sjson 路径里的保留字符，使含 . * ? 的头名也能精确定位。
func sjsonEscapePathKey(key string) string {
	return strings.NewReplacer(".", `\.`, "*", `\*`, "?", `\?`).Replace(key)
}

// scrubCodexMetadataHeadersInEvent 按白名单裁剪元数据事件的 headers 对象。
// 载荷里没有该对象、或整块透传开启时原样返回。
func scrubCodexMetadataHeadersInEvent(payload []byte) []byte {
	if codexMetadataHeaderPassthroughEnabled() {
		return payload
	}
	headers := gjson.GetBytes(payload, "headers")
	if !headers.IsObject() {
		return payload
	}
	drop := make([]string, 0, 8)
	headers.ForEach(func(key, _ gjson.Result) bool {
		name := strings.ToLower(strings.TrimSpace(key.String()))
		if _, keep := codexDownstreamMetadataHeaderAllowlist[name]; !keep {
			drop = append(drop, key.String())
		}
		return true
	})
	if len(drop) == 0 {
		return payload
	}
	out := payload
	for _, key := range drop {
		if updated, err := sjson.DeleteBytes(out, "headers."+sjsonEscapePathKey(key)); err == nil {
			out = updated
		}
	}
	return out
}
