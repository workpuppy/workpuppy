// accesskey.go 数据面鉴权：下游接入 key → 请求上下文中的授权范围。
//
// 与旧 config.api_key 的区别：
//   - 旧 key 只有「有 / 无」两态，空值等于不鉴权；
//   - 现在 /v1、/status 只认「接入映射」里创建并启用的 key；未配置任何 key 时一律
//     401（显式失败，避免"以为开了鉴权其实裸奔"）。每把 key 自带账号/模型白名单，
//     由此处写入请求上下文，后续选号、粘性、模型清单各自读取。
//
// 管理面**不**走这里：/admin/* 只认管理密码（admin_login.go），两套凭据互不相通。
package server

import (
	"context"
	"net/http"
	"strings"

	"wp/internal/accesskey"
	"wp/internal/pool"
)

// accessKeyContextKey 上下文键（空结构体，避免与其它包的键碰撞）。
type accessKeyContextKey struct{}

// withAccessKeyContext 把命中的 key 放进请求上下文。
func withAccessKeyContext(r *http.Request, k *accesskey.Key) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), accessKeyContextKey{}, k))
}

// accessKeyOf 取当前请求的接入 key；nil = 非数据面路径（管理面测试台、内部调用）。
//
// 管理面 /admin/playground/chat 有意不注入 key：它要验证的是「整池链路 + 指定账号」，
// 不应被某把下游 key 的白名单限制；调用方用管理密码鉴权，已具备全量权限。
func accessKeyOf(r *http.Request) *accesskey.Key {
	if r == nil {
		return nil
	}
	k, _ := r.Context().Value(accessKeyContextKey{}).(*accesskey.Key)
	return k
}

// scopeOfKey 把接入 key 的账号白名单转成选号范围。
//
//   - key == nil（内部/管理面）→ nil = 不限；
//   - AllAccounts → nil = 不限（realm 由 keyRealm + 选号时的模型域过滤）；
//   - 否则返回 UIDSet：空列表得到空集合 = 全部拒绝（不是 nil！）。
func scopeOfKey(k *accesskey.Key) pool.Scope {
	if k == nil || k.AllAccounts {
		return nil
	}
	return pool.NewUIDSet(k.Accounts)
}

// keyRealm 返回 key 的版本（cn/global）；加载期已把空值归一为 cn，
// 这里再兜一层，避免测试零值/手写文件导致空串。
func keyRealm(k *accesskey.Key) string {
	if k != nil && k.Realm == "global" {
		return "global"
	}
	return "cn"
}

// bearerToken 提取 Authorization: Bearer <token>；缺失/空返回 false。
// 只认 Bearer 方案（与 OpenAI SDK 一致），不接受裸 token。
func bearerToken(r *http.Request) (string, bool) {
	authz := r.Header.Get("Authorization")
	if !strings.HasPrefix(authz, "Bearer ") {
		return "", false
	}
	token := strings.TrimSpace(strings.TrimPrefix(authz, "Bearer "))
	if token == "" {
		return "", false
	}
	return token, true
}

// AccessKeys nil = 未接线（测试与纯内部嵌入）：不做数据面鉴权。生产入口
// cmd/server 恒注入非 nil 存储——即使 access-keys.json 不存在，也是空存储 →
// 所有 /v1 请求 401。这样既保留既有测试的「裸 handler」写法，又不会让正式部署
// 因为漏配而变成不鉴权。
//
// 来源与用途见 Config.AccessKeys。
func (h *Handler) withAccessKey(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.cfg.AccessKeys == nil {
			next(w, r)
			return
		}
		token, ok := bearerToken(r)
		if !ok {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
			return
		}
		k, ok := h.cfg.AccessKeys.Lookup(token)
		if !ok || !k.Enabled {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
			return
		}
		next(w, withAccessKeyContext(r, &k))
	}
}
