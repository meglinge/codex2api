package proxy

import (
	"strconv"
	"strings"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ==================== 输出项 / call_id 映射 ====================
//
// 加密推理必须随原 item id 回传，所以这些标识不能删、也不能只改写：下游拿到
// 网关签发的同前缀 id，回带时换回上游 id。blob 原样。
//
// 扫描所有名为 id / item_id / call_id 的字符串字段。前缀规则与源码
// ResponseItemId::is_prefixed 一致：第一段非空、有 '_'、后缀非空。resp_ 与
// c2a. 由其它表处理。签发形状与 ResponseItemId::new 一致：{prefix}_{UUIDv7}。

var codexItemIDFieldNames = map[string]struct{}{
	"id":      {},
	"item_id": {},
	"call_id": {},
}

func codexItemIDPrefix(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || strings.HasPrefix(value, codexResponseIDPrefix) || strings.HasPrefix(value, codexTurnStateTokenPrefix) {
		return ""
	}
	idx := strings.IndexByte(value, '_')
	if idx <= 0 || idx == len(value)-1 {
		return ""
	}
	for _, r := range value[:idx] {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return ""
		}
	}
	return value[:idx+1]
}

func downstreamCodexItemID(upstream string) string {
	upstream = strings.TrimSpace(upstream)
	prefix := codexItemIDPrefix(upstream)
	if prefix == "" {
		return upstream
	}
	now := codexIDIsolationNow()
	if raw, ok := codexItemIDUpToDown.Load(upstream); ok {
		if entry, ok := raw.(*codexResponseIDEntry); ok && now.Before(entry.expiresAt) {
			return entry.value
		}
	}
	if rec, ok := loadCodexIDMap(codexIDMapNSItemUp, upstream); ok {
		expires := now.Add(codexItemIDMapTTL)
		codexItemIDUpToDown.Store(upstream, &codexResponseIDEntry{value: rec.Value, expiresAt: expires})
		codexItemIDDownToUp.Store(rec.Value, &codexResponseIDEntry{value: upstream, expiresAt: expires})
		return rec.Value
	}
	seed := "codex2api:item-id:v2:" + prefix + ":" + upstream
	downstream := prefix + deriveStableCodexUUIDv7(seed, seededIdentityUnixMilli(seed))
	expires := now.Add(codexItemIDMapTTL)
	codexItemIDUpToDown.Store(upstream, &codexResponseIDEntry{value: downstream, expiresAt: expires})
	codexItemIDDownToUp.Store(downstream, &codexResponseIDEntry{value: upstream, expiresAt: expires})
	sweepCodexIDMap(&codexItemIDUpToDown, &codexItemIDWrites)
	sweepCodexIDMap(&codexItemIDDownToUp, &codexItemIDWrites)
	persistCodexIDMap(codexIDMapNSItemUp, upstream, codexIDMapRecord{Value: downstream}, codexItemIDMapTTL)
	persistCodexIDMap(codexIDMapNSItemDown, downstream, codexIDMapRecord{Value: upstream}, codexItemIDMapTTL)
	return downstream
}

func upstreamCodexItemID(downstream string) (string, bool) {
	downstream = strings.TrimSpace(downstream)
	raw, ok := codexItemIDDownToUp.Load(downstream)
	if !ok {
		rec, found := loadCodexIDMap(codexIDMapNSItemDown, downstream)
		if !found {
			return "", false
		}
		expires := codexIDIsolationNow().Add(codexItemIDMapTTL)
		codexItemIDDownToUp.Store(downstream, &codexResponseIDEntry{value: rec.Value, expiresAt: expires})
		codexItemIDUpToDown.Store(rec.Value, &codexResponseIDEntry{value: downstream, expiresAt: expires})
		return rec.Value, true
	}
	entry, ok := raw.(*codexResponseIDEntry)
	if !ok || codexIDIsolationNow().After(entry.expiresAt) {
		return "", false
	}
	return entry.value, true
}

func rewriteDownstreamItemIDs(payload []byte) []byte {
	return rewriteCodexIDFields(payload, func(value string) string {
		if codexItemIDPrefix(value) == "" {
			return value
		}
		if _, ok := upstreamCodexItemID(value); ok {
			return value
		}
		return downstreamCodexItemID(value)
	})
}

func mapUpstreamItemIDsInRequest(body []byte, _ *auth.Account) []byte {
	return rewriteCodexIDFields(body, func(value string) string {
		if upstream, ok := upstreamCodexItemID(value); ok {
			return upstream
		}
		return value
	})
}

func rewriteCodexIDFields(payload []byte, rewrite func(string) string) []byte {
	trimmed := strings.TrimSpace(string(payload))
	if trimmed == "" || trimmed[0] != '{' || !gjson.ValidBytes(payload) {
		return payload
	}
	return walkRewriteJSONStrings(payload, "", gjson.ParseBytes(payload), rewrite)
}

func walkRewriteJSONStrings(payload []byte, prefix string, node gjson.Result, rewrite func(string) string) []byte {
	switch {
	case node.IsObject():
		node.ForEach(func(key, value gjson.Result) bool {
			name := key.String()
			path := sjsonEscapePathKey(name)
			if prefix != "" {
				path = prefix + "." + path
			}
			if value.Type == gjson.String {
				if _, named := codexItemIDFieldNames[name]; named {
					next := rewrite(value.String())
					if next != "" && next != value.String() {
						if updated, err := sjson.SetBytes(payload, path, next); err == nil {
							payload = updated
						}
					}
				}
				return true
			}
			if value.IsObject() || value.IsArray() {
				payload = walkRewriteJSONStrings(payload, path, value, rewrite)
			}
			return true
		})
	case node.IsArray():
		i := 0
		node.ForEach(func(_, value gjson.Result) bool {
			path := strconv.Itoa(i)
			if prefix != "" {
				path = prefix + "." + path
			}
			if value.IsObject() || value.IsArray() {
				payload = walkRewriteJSONStrings(payload, path, value, rewrite)
			}
			i++
			return true
		})
	}
	return payload
}
