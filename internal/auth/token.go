// Package auth 提供对宿主交付的 OAuth 访问令牌的最小只读解析能力。
//
// Sub2API 负责账号选择与 OAuth Token 生命周期，正常情况下会在转发头里带上
// Authorization 和 chatgpt-account-id。本包只做一件事：当 chatgpt-account-id
// 头部缺失时，从访问令牌的 JWT 声明中回退解析账号 ID，避免因为下游客户端
// 没有透传该头部而直接失败。它不刷新、不持久化任何令牌。
package auth

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
)

// Identity 是从访问令牌中提取出的只读身份信息。
type Identity struct {
	AccessToken string
	AccountID   string
	Email       string
	ExpiresAt   time.Time
}

// StripBearer 从 Authorization 头中取出裸令牌。
func StripBearer(header string) string {
	value := strings.TrimSpace(header)
	if len(value) >= 7 && strings.EqualFold(value[:7], "bearer ") {
		value = value[7:]
	}
	return strings.TrimSpace(value)
}

// ParseToken 解析访问令牌，并在令牌未携带账号 ID 时沿用回退值。
//
// 账号 ID 优先取宿主透传值：Sub2API 才是账号权威来源，影子账号会刻意透传母账号的
// chatgpt-account-id，此时令牌里的子账号 ID 不应覆盖它。只有宿主没有提供时才回退到
// 访问令牌的 JWT 声明。
func ParseToken(accessToken string, hostAccountID string) Identity {
	accessToken = StripBearer(accessToken)
	claims := jwtPayload(accessToken)
	identity := Identity{AccessToken: accessToken, AccountID: strings.TrimSpace(hostAccountID)}
	if identity.AccountID == "" {
		identity.AccountID = accountIDFromClaims(claims)
	}
	identity.Email = stringValue(claims["email"])
	identity.ExpiresAt = timeFromValue(claims["exp"])
	return identity
}

// Expired 判断令牌是否已经过期。零值表示没有可用的过期时间，视为未过期。
func (i Identity) Expired(now time.Time) bool {
	return !i.ExpiresAt.IsZero() && !now.Before(i.ExpiresAt)
}

func jwtPayload(token string) map[string]any {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return map[string]any{}
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		data, err = base64.URLEncoding.DecodeString(parts[1])
	}
	if err != nil {
		return map[string]any{}
	}
	var claims map[string]any
	if json.Unmarshal(data, &claims) != nil || claims == nil {
		return map[string]any{}
	}
	return claims
}

func accountIDFromClaims(claims map[string]any) string {
	if auth, ok := claims["https://api.openai.com/auth"].(map[string]any); ok {
		for _, key := range []string{"chatgpt_account_id", "account_id", "chatgpt_account_user_id"} {
			if value := stringValue(auth[key]); value != "" {
				return value
			}
		}
	}
	for _, key := range []string{"chatgpt_account_id", "account_id"} {
		if value := stringValue(claims[key]); value != "" {
			return value
		}
	}
	return ""
}

func stringValue(value any) string {
	s, _ := value.(string)
	return strings.TrimSpace(s)
}

func timeFromValue(value any) time.Time {
	switch typed := value.(type) {
	case json.Number:
		if seconds, err := typed.Int64(); err == nil && seconds > 0 {
			return time.Unix(seconds, 0)
		}
	case float64:
		if typed > 0 {
			return time.Unix(int64(typed), 0)
		}
	case string:
		if timestamp, err := time.Parse(time.RFC3339, strings.TrimSpace(typed)); err == nil {
			return timestamp
		}
	}
	return time.Time{}
}
