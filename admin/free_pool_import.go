package admin

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

const freePoolImportMaxBody = 8 << 20

type freePoolImportAccount struct {
	Name        string
	ProxyURL    string
	Credentials json.RawMessage
}

// parseFreePoolImportAccounts 复用普通账号的 CPA / sub2api / 平铺 JSON 解析。
// 号池只收带 access_token 和 account_id 的 Codex OAuth，不收 API key 或 Agent Identity。
func parseFreePoolImportAccounts(data []byte) ([]freePoolImportAccount, error) {
	tokens, err := parseImportJSONTokens(data)
	if err != nil {
		return nil, err
	}
	accounts := make([]freePoolImportAccount, 0, len(tokens))
	for _, token := range tokens {
		accountID := freePoolImportAccountID(token)
		if token.agentRuntimeID != "" || strings.TrimSpace(token.accessToken) == "" || accountID == "" {
			continue
		}
		credentials, err := json.Marshal(map[string]string{
			"access_token": strings.TrimSpace(token.accessToken),
			"account_id":   accountID,
		})
		if err != nil || len(credentials) > 64*1024 {
			continue
		}
		name := firstNonEmpty(token.name, token.email, accountID)
		accounts = append(accounts, freePoolImportAccount{
			Name:        name,
			ProxyURL:    strings.TrimSpace(token.proxyURL),
			Credentials: credentials,
		})
	}
	return accounts, nil
}

// freePoolImportAccountID 优先用导出里的工作区 ID。个人 free 号常常不写
// chatgpt_account_id，只在 id_token 的默认组织里带 org-*。user-* 不能当工作区。
func freePoolImportAccountID(token importToken) string {
	if id := strings.TrimSpace(token.chatgptAccountID); workspaceID(id) {
		return id
	}
	if info := auth.ParseIDToken(token.idToken); info != nil && workspaceID(info.ChatGPTAccountID) {
		return strings.TrimSpace(info.ChatGPTAccountID)
	}
	if info := auth.ParseAccessToken(token.accessToken); info != nil && workspaceID(info.ChatGPTAccountID) {
		return strings.TrimSpace(info.ChatGPTAccountID)
	}
	return freePoolDefaultOrgID(token.idToken)
}

func workspaceID(id string) bool {
	id = strings.TrimSpace(id)
	return id != "" && !strings.HasPrefix(strings.ToLower(id), "user-")
}

func freePoolDefaultOrgID(idToken string) string {
	parts := strings.Split(strings.TrimSpace(idToken), ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Auth *struct {
			Organizations []struct {
				ID        string `json:"id"`
				IsDefault bool   `json:"is_default"`
			} `json:"organizations"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Auth == nil {
		return ""
	}
	var fallback string
	for _, org := range claims.Auth.Organizations {
		id := strings.TrimSpace(org.ID)
		if !workspaceID(id) {
			continue
		}
		if org.IsDefault {
			return id
		}
		if fallback == "" {
			fallback = id
		}
	}
	return fallback
}

func (h *Handler) ImportFreePoolAccounts(c *gin.Context) {
	if err := c.Request.ParseMultipartForm(freePoolImportMaxBody); err != nil && err != http.ErrNotMultipart {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid free pool request"})
		return
	}
	var raw []byte
	var err error
	if file, header, fileErr := c.Request.FormFile("file"); fileErr == nil {
		defer file.Close()
		if header.Size > freePoolImportMaxBody {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid free pool request"})
			return
		}
		raw, err = io.ReadAll(io.LimitReader(file, freePoolImportMaxBody+1))
	} else {
		raw, err = io.ReadAll(io.LimitReader(c.Request.Body, freePoolImportMaxBody+1))
	}
	if err != nil || len(raw) == 0 || len(raw) > freePoolImportMaxBody {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid free pool request"})
		return
	}
	status := strings.TrimSpace(c.PostForm("status"))
	if status == "" {
		status = strings.TrimSpace(c.Query("status"))
	}
	if status == "" {
		var wrapped struct {
			Status string `json:"status"`
		}
		_ = json.Unmarshal(bytes.TrimSpace(raw), &wrapped)
		status = strings.TrimSpace(wrapped.Status)
	}
	if status == "" {
		status = "disabled"
	}
	if status != "active" && status != "disabled" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid free pool request"})
		return
	}
	parsed, err := parseFreePoolImportAccounts(raw)
	if err != nil || len(parsed) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid free pool request"})
		return
	}
	created := make([]int64, 0, len(parsed))
	skipped := 0
	for _, account := range parsed {
		id, err := h.db.CreateFreePoolAccount(c.Request.Context(), database.FreePoolAccountInput{
			Name: account.Name, Status: status, ProxyURL: account.ProxyURL, Credentials: account.Credentials,
		})
		if err != nil {
			skipped++
			continue
		}
		created = append(created, id)
	}
	if len(created) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid free pool request", "created": 0, "skipped": skipped})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"created": len(created), "ids": created, "skipped": skipped})
}
