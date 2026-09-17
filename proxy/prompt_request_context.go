package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/codex2api/database"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const promptRequestSecurityContextKey = "prompt_filter_request_security_context"
const promptPolicyRequestCorrelationContextKey = "prompt_policy_request_correlation_id"

// promptRequestSecurityContext owns request-local prompt security state. It is
// deliberately separate from the verified NewAPI identity keys because an HTTP
// request has one body/config snapshot while a WebSocket connection can carry
// multiple logical request frames under one verified connection identity.
type promptRequestSecurityContext struct {
	configOwner        *Handler
	config             promptfilter.Config
	configReady        bool
	digestBody         []byte
	digest             [sha256.Size]byte
	digestHex          string
	digestReady        bool
	digestComputations uint8
}

func promptRequestSecurityState(c *gin.Context) *promptRequestSecurityContext {
	if c == nil {
		return nil
	}
	if value, ok := c.Get(promptRequestSecurityContextKey); ok {
		if state, valid := value.(*promptRequestSecurityContext); valid && state != nil {
			return state
		}
	}
	state := &promptRequestSecurityContext{}
	c.Set(promptRequestSecurityContextKey, state)
	return state
}

// resetPromptRequestSecurityFrame starts a fresh per-frame config/digest scope.
// The verified NewAPI identity remains connection-scoped, while the WebSocket
// turn boundary first refreshes/revokes its API-key binding. This keeps policy
// changes hot without letting a removed tenant or expired secret survive on an
// old connection.
func resetPromptRequestSecurityFrame(c *gin.Context) {
	if c != nil {
		c.Set(promptRequestSecurityContextKey, &promptRequestSecurityContext{})
		// A WebSocket connection carries multiple logical requests. Never let a
		// prior turn's upstream CYB decision leak into the next turn.
		c.Set(newAPIUpstreamCyberDecisionContextKey, nil)
	}
}

// releasePromptRequestFrameBody drops turn-local payload references before a
// WebSocket waits for its next message. Connection identity and API-key binding
// remain available across turns; request bodies and body digests must not.
func releasePromptRequestFrameBody(c *gin.Context) {
	if c == nil {
		return
	}
	c.Set("raw_body", nil)
	c.Set(ingressRequestBodyContextKey, nil)
	c.Set(promptRequestSecurityContextKey, nil)
}

func (h *Handler) promptFilterConfigForRequest(c *gin.Context) promptfilter.Config {
	cfg := h.promptFilterBaseConfigForRequest(c)
	// 用户级豁免不进请求级缓存：capturePromptRequestIngress 会在 body 存进上下文
	// 之前先解析一次配置，那一刻还验不了签名；等身份验证通过后再问，答案才对。
	if cfg.Enabled && h.promptFilterNewAPIUserExempt(c, cfg) {
		cfg.Enabled = false
	}
	return cfg
}

// promptFilterNewAPIUserExempt 判断当前请求的 NewAPI 用户是否在绑定的豁免名单里。
// 只认签名验证通过的身份：未签名、验签失败或还拿不到 body 时一律不豁免。
func (h *Handler) promptFilterNewAPIUserExempt(c *gin.Context, cfg promptfilter.Config) bool {
	if c == nil || h == nil || h.store == nil {
		return false
	}
	binding, bound := h.resolvePromptFilterNewAPIBinding(c)
	if !bound || !binding.Enabled || len(binding.ExemptUserIDs) == 0 {
		return false
	}
	apiKeyID := requestAPIKeyID(c)
	if value, exists := c.Get(newAPIIdentityContextKey); exists {
		// HTTP 在守卫评估里、WS 在握手时已经验过签名，直接复用。
		if identity, ok := value.(verifiedNewAPIIdentityContext); ok && identity.APIKeyID == apiKeyID {
			return promptFilterBindingExemptsUser(binding, identity.Identity.UserID)
		}
	}
	body := ingressRequestBody(c, nil)
	if body == nil {
		return false
	}
	identity, verified := h.verifyNewAPIIdentityContext(c, cfg.Advanced.NewAPI, body)
	return verified && identity.APIKeyID == apiKeyID && promptFilterBindingExemptsUser(binding, identity.Identity.UserID)
}

func promptFilterBindingExemptsUser(binding database.PromptFilterNewAPIBinding, userID string) bool {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return false
	}
	for _, exempt := range binding.ExemptUserIDs {
		if strings.TrimSpace(exempt) == userID {
			return true
		}
	}
	return false
}

// promptFilterBaseConfigForRequest 解析并缓存请求级配置：全局快照 + API Key 的
// NewAPI 绑定收窄 + 分组范围闸门。用户级豁免在 promptFilterConfigForRequest 里另算。
func (h *Handler) promptFilterBaseConfigForRequest(c *gin.Context) promptfilter.Config {
	if h == nil || h.store == nil {
		return promptfilter.DefaultConfig()
	}
	state := promptRequestSecurityState(c)
	if state == nil {
		return h.store.GetPromptFilterConfigSnapshot()
	}
	if state.configReady && state.configOwner == h {
		return state.config
	}
	state.config = h.store.GetPromptFilterConfigSnapshot()
	// Signed NewAPI identity is available only through an explicit API-key
	// binding. Ignore any retired persisted enablement value.
	state.config.Advanced.NewAPI.Enabled = false
	if binding, bound := h.resolvePromptFilterNewAPIBinding(c); bound {
		state.config.Advanced.NewAPI.Enabled = binding.Enabled
		if binding.Enabled {
			switch binding.PromptFilterScope {
			case database.PromptFilterScopeLocalOnly:
				// Keep the deterministic local GuardPipeline, session correlation,
				// audit evidence and risk profiling, but remove the synchronous
				// remote model hop for this API key.
				state.config.Review.Enabled = false
			case database.PromptFilterScopeOff:
				// This explicit admin-side API-key exception disables Prompt checks
				// only. NewAPI signature verification remains enabled above and API
				// key authentication is enforced before this request-local snapshot.
				state.config.Enabled = false
			}
		}
	}
	// 分组范围是全局闸门：Key 没绑定到圈定的分组就不检查。放在 NewAPI 绑定之后，
	// 绑定只能在范围内进一步收窄（local_only / off），不能把范围外的 Key 拉回来。
	if state.config.Enabled && !h.promptFilterScopeCoversRequest(c, state.config.Advanced.Scope) {
		state.config.Enabled = false
	}
	state.configOwner = h
	state.configReady = true
	return state.config
}

// promptFilterScopeCoversRequest 按当前请求 API Key 绑定的账号分组判断是否在
// Prompt 检查范围内。没有 API Key 身份的请求按「未绑定分组」处理。
func (h *Handler) promptFilterScopeCoversRequest(c *gin.Context, scope promptfilter.ScopeConfig) bool {
	if !scope.Restricted() {
		return true
	}
	var groupIDs []int64
	if apiKeyID := requestAPIKeyID(c); apiKeyID > 0 && h != nil && h.store != nil {
		// store 侧的允许组是权威源：分组被删除后会同步刷新。
		groupIDs = h.store.GetAPIKeyAllowedGroups(apiKeyID)
	} else if row := apiKeyRowFromContext(c); row != nil {
		groupIDs = row.AllowedGroupIDs
	}
	return scope.CoversAPIKey(groupIDs)
}

// capturePromptRequestIngress retains the already-owned request buffer by
// reference when signed NewAPI verification or a Codex-local conversation lock
// can need the pre-mapping body. The latter is required for clients whose only
// stable session signal lives in client_metadata. Callers must treat the buffer
// as immutable; body rewrites use a new slice.
func (h *Handler) capturePromptRequestIngress(c *gin.Context, body []byte) {
	if c == nil {
		return
	}
	ensurePromptPolicyRequestCorrelationID(c)
	if h == nil || h.store == nil {
		return
	}
	cfg := h.promptFilterConfigForRequest(c)
	needsSignedBody := cfg.Advanced.NewAPI.Enabled && strings.TrimSpace(c.GetHeader("X-NewAPI-Signature")) != ""
	needsFallbackSessionBody := cfg.Enabled && cfg.Advanced.Enforcement.ConversationLockEnabled
	if !needsSignedBody && !needsFallbackSessionBody {
		return
	}
	setIngressRequestBodyIfAbsent(c, body)
}

func ensurePromptPolicyRequestCorrelationID(c *gin.Context) string {
	if c == nil {
		return ""
	}
	if raw, exists := c.Get(promptPolicyRequestCorrelationContextKey); exists {
		if value, ok := raw.(string); ok && strings.TrimSpace(value) != "" {
			return value
		}
	}
	value := uuid.NewString()
	c.Set(promptPolicyRequestCorrelationContextKey, value)
	return value
}

func resetPromptPolicyRequestCorrelationID(c *gin.Context) string {
	if c == nil {
		return ""
	}
	value := uuid.NewString()
	c.Set(promptPolicyRequestCorrelationContextKey, value)
	return value
}

func sameRequestBodyBuffer(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	if len(left) == 0 {
		return true
	}
	return &left[0] == &right[0]
}

func promptRequestBodyDigest(c *gin.Context, body []byte) ([sha256.Size]byte, string) {
	state := promptRequestSecurityState(c)
	if state != nil && state.digestReady && sameRequestBodyBuffer(state.digestBody, body) {
		return state.digest, state.digestHex
	}
	digest := sha256.Sum256(body)
	digestHex := hex.EncodeToString(digest[:])
	if state != nil {
		state.digestBody = body
		state.digest = digest
		state.digestHex = digestHex
		state.digestReady = true
		state.digestComputations++
	}
	return digest, digestHex
}

func promptRequestDigestComputationCount(c *gin.Context) int {
	state := promptRequestSecurityState(c)
	if state == nil {
		return 0
	}
	return int(state.digestComputations)
}
