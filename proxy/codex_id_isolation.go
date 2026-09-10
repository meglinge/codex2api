package proxy

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ==================== 上下游标识隔离（映射表） ====================
//
// 上游签发、下游需要回带的两个标识不再原样透传，而是由网关签发自己的值并在
// 本地保留映射：
//
//   - x-codex-turn-state：上游为账号铸造的粘性路由 blob。下游只拿到网关 token
//     （c2a.<随机>），回带时查表：账号一致就换回上游 blob，不一致或过期直接丢弃，
//     跨账号回带的矛盾信号从此不可能到达上游。blob 出现在 HTTP 响应头，也出现在
//     WS 传输路径的 codex.response.metadata 事件的 headers 里；回带则在 HTTP 请求头
//     或 WS 帧的 client_metadata.x-codex-turn-state 里，四处都经过这里。
//   - response.id：下游拿到网关签发的 resp_<随机>，本地缓存、续链、日志全部以它为键。
//     HTTP 路径出站前本就剥离 previous_response_id（历史在本地展开），上游 id 用完即弃；
//     WS 路径续链要把 previous_response_id 发回产出它的那条连接，出站前按映射换回
//     上游 id。同一次响应的 created / in_progress / completed 事件共用一个下游 id。
//
// 两张表都是进程内存并带 TTL：turn-state 的粘性本来就限于单实例，WS 续链上下文也
// 只存活在本进程持有的连接里，跨实例本就不成立；HTTP 路径不依赖表。

const (
	codexTurnStateTokenPrefix = "c2a."
	codexTurnStateTokenTTL    = time.Hour
	codexResponseIDPrefix     = "resp_"
	codexResponseIDMapTTL     = 24 * time.Hour
	codexIDMapSweepEvery      = 256
)

type codexTurnStateEntry struct {
	accountID int64
	upstream  string
	expiresAt time.Time
}

type codexResponseIDEntry struct {
	value     string
	expiresAt time.Time
}

var (
	codexTurnStateTokens      sync.Map // token -> *codexTurnStateEntry
	codexTurnStateTokenWrites atomic.Uint64
	codexResponseIDDownToUp   sync.Map // downstream id -> *codexResponseIDEntry(upstream)
	codexResponseIDUpToDown   sync.Map // upstream id -> *codexResponseIDEntry(downstream)
	codexResponseIDWrites     atomic.Uint64
	codexIDIsolationNow       = time.Now
)

func codexRandomHex(bytes int) string {
	buf := make([]byte, bytes)
	if _, err := rand.Read(buf); err != nil {
		// 熵源不可用时退回 UUIDv7 的随机位，仍保证唯一。
		return strings.ReplaceAll(NewUpstreamSessionUUID(), "-", "")
	}
	return hex.EncodeToString(buf)
}

func sweepCodexIDMap(m *sync.Map, counter *atomic.Uint64) {
	if counter.Add(1)%codexIDMapSweepEvery != 0 {
		return
	}
	now := codexIDIsolationNow()
	m.Range(func(key, value any) bool {
		switch entry := value.(type) {
		case *codexTurnStateEntry:
			if now.After(entry.expiresAt) {
				m.Delete(key)
			}
		case *codexResponseIDEntry:
			if now.After(entry.expiresAt) {
				m.Delete(key)
			}
		default:
			m.Delete(key)
		}
		return true
	})
}

// ---------- turn-state ----------

func isCodexTurnStateToken(value string) bool {
	return strings.HasPrefix(strings.TrimSpace(value), codexTurnStateTokenPrefix)
}

// mintCodexTurnStateToken 为上游 blob 签发下游 token 并记账号归属。账号缺失时不签发，
// 返回空串（调用方不下发该头）。
func mintCodexTurnStateToken(account *auth.Account, upstream string) string {
	upstream = strings.TrimSpace(upstream)
	if upstream == "" || account == nil || account.ID() <= 0 {
		return ""
	}
	token := codexTurnStateTokenPrefix + codexRandomHex(24)
	codexTurnStateTokens.Store(token, &codexTurnStateEntry{
		accountID: account.ID(),
		upstream:  upstream,
		expiresAt: codexIDIsolationNow().Add(codexTurnStateTokenTTL),
	})
	sweepCodexIDMap(&codexTurnStateTokens, &codexTurnStateTokenWrites)
	return token
}

// resolveCodexTurnStateToken 把下游回带的 token 换回上游 blob；未知、过期或账号不符
// 都返回 ok=false。
func resolveCodexTurnStateToken(token string, account *auth.Account) (string, bool) {
	raw, found := codexTurnStateTokens.Load(strings.TrimSpace(token))
	if !found {
		return "", false
	}
	entry, ok := raw.(*codexTurnStateEntry)
	if !ok || codexIDIsolationNow().After(entry.expiresAt) {
		codexTurnStateTokens.Delete(strings.TrimSpace(token))
		return "", false
	}
	if account == nil || entry.accountID != account.ID() {
		return "", false
	}
	return entry.upstream, true
}

// resolveCodexTurnStateEcho 在出站前把请求头与请求体里下游回带的 token 换回上游 blob：
// 账号一致换值，否则删除。非 token 的原始值（升级前签发的 blob）保持原样，交给既有的
// 溯源守卫处理。请求头有改动时返回克隆。
func resolveCodexTurnStateEcho(account *auth.Account, body []byte, headers http.Header) ([]byte, http.Header) {
	if headers != nil {
		if value := strings.TrimSpace(headers.Get(codexTurnStateHeader)); isCodexTurnStateToken(value) {
			headers = headers.Clone()
			if upstream, ok := resolveCodexTurnStateToken(value, account); ok {
				headers.Set(codexTurnStateHeader, upstream)
			} else {
				headers.Del(codexTurnStateHeader)
			}
		}
	}
	const path = "client_metadata.x-codex-turn-state"
	if field := gjson.GetBytes(body, path); field.Type == gjson.String && isCodexTurnStateToken(field.String()) {
		if upstream, ok := resolveCodexTurnStateToken(field.String(), account); ok {
			if updated, err := sjson.SetBytes(body, path, upstream); err == nil {
				body = updated
			}
		} else if updated, err := sjson.DeleteBytes(body, path); err == nil {
			body = updated
		}
	}
	return body, headers
}

// rewriteCodexTurnStateInEvent 把 WS 传输路径元数据事件（codex.response.metadata 等）
// headers 里的上游 blob 换成下游 token。没有该字段或无法签发时原样返回。
func rewriteCodexTurnStateInEvent(payload []byte, account *auth.Account) []byte {
	const path = "headers.x-codex-turn-state"
	field := gjson.GetBytes(payload, path)
	if field.Type != gjson.String || field.String() == "" || isCodexTurnStateToken(field.String()) {
		return payload
	}
	token := mintCodexTurnStateToken(account, field.String())
	if token == "" {
		if updated, err := sjson.DeleteBytes(payload, path); err == nil {
			return updated
		}
		return payload
	}
	if updated, err := sjson.SetBytes(payload, path, token); err == nil {
		return updated
	}
	return payload
}

// ---------- response.id ----------

// downstreamCodexResponseID 返回上游 response id 对应的下游 id，首次出现时签发。
func downstreamCodexResponseID(upstream string) string {
	upstream = strings.TrimSpace(upstream)
	if upstream == "" {
		return ""
	}
	now := codexIDIsolationNow()
	if raw, ok := codexResponseIDUpToDown.Load(upstream); ok {
		if entry, ok := raw.(*codexResponseIDEntry); ok && now.Before(entry.expiresAt) {
			return entry.value
		}
	}
	downstream := codexResponseIDPrefix + codexRandomHex(25)
	expires := now.Add(codexResponseIDMapTTL)
	codexResponseIDUpToDown.Store(upstream, &codexResponseIDEntry{value: downstream, expiresAt: expires})
	codexResponseIDDownToUp.Store(downstream, &codexResponseIDEntry{value: upstream, expiresAt: expires})
	sweepCodexIDMap(&codexResponseIDUpToDown, &codexResponseIDWrites)
	sweepCodexIDMap(&codexResponseIDDownToUp, &codexResponseIDWrites)
	return downstream
}

// upstreamCodexResponseID 把下游 id 换回上游 id；未知时返回 ok=false。
func upstreamCodexResponseID(downstream string) (string, bool) {
	raw, ok := codexResponseIDDownToUp.Load(strings.TrimSpace(downstream))
	if !ok {
		return "", false
	}
	entry, ok := raw.(*codexResponseIDEntry)
	if !ok || codexIDIsolationNow().After(entry.expiresAt) {
		return "", false
	}
	return entry.value, true
}

// rewriteDownstreamResponseID 把发往下游的 Responses 载荷里的 response id 换成网关 id：
// SSE 事件的 response.id，或裸 response 对象的顶层 id。其余字段不动。
func rewriteDownstreamResponseID(payload []byte) []byte {
	trimmed := strings.TrimSpace(string(payload))
	if trimmed == "" || trimmed[0] != '{' || !gjson.ValidBytes(payload) {
		return payload
	}
	path := ""
	switch {
	case gjson.GetBytes(payload, "response.id").Type == gjson.String:
		path = "response.id"
	case gjson.GetBytes(payload, "object").String() == "response" && gjson.GetBytes(payload, "id").Type == gjson.String:
		path = "id"
	default:
		return payload
	}
	upstream := gjson.GetBytes(payload, path).String()
	if upstream == "" || strings.HasPrefix(upstream, codexResponseIDPrefix) && upstreamKnownDownstream(upstream) {
		return payload
	}
	downstream := downstreamCodexResponseID(upstream)
	if downstream == "" || downstream == upstream {
		return payload
	}
	if updated, err := sjson.SetBytes(payload, path, downstream); err == nil {
		return updated
	}
	return payload
}

// upstreamKnownDownstream 报告一个 id 是否已是网关签发的下游 id（重放/二次处理时幂等）。
func upstreamKnownDownstream(id string) bool {
	_, ok := upstreamCodexResponseID(id)
	return ok
}

// mapPreviousResponseIDToUpstream 把请求体里下游回带的 previous_response_id 换回上游 id。
// 未知的 id（升级前签发、或来自别处的上游 id）保持原样，交给上游裁决。
func mapPreviousResponseIDToUpstream(body []byte) []byte {
	field := gjson.GetBytes(body, "previous_response_id")
	if field.Type != gjson.String || field.String() == "" {
		return body
	}
	upstream, ok := upstreamCodexResponseID(field.String())
	if !ok || upstream == field.String() {
		return body
	}
	if updated, err := sjson.SetBytes(body, "previous_response_id", upstream); err == nil {
		return updated
	}
	return body
}
