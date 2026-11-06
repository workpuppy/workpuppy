// config.go 加载 JSON 配置 + 环境变量覆盖。
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/adminauth"
	"workbuddy2api/internal/config"
	"workbuddy2api/internal/configdata"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/prompt"
	"workbuddy2api/internal/redisstore"
	"workbuddy2api/internal/server"
	"workbuddy2api/internal/upstream"
)

// defaultCfg/defaultCfgOnce 缓存"纯默认配置"，供 normalize 的兜底取值使用。
var (
	defaultCfg     *Config
	defaultCfgOnce sync.Once
)

// Config 顶层配置。
type Config struct {
	Listen    string `json:"listen"`     // ":7863"
	APIKey    string `json:"api_key"`    // 空 = 不鉴权
	AuthDir   string `json:"auth_dir"`   // ./auths
	StateFile string `json:"state_file"` // ./data/state.json

	// Proxies 账号级出口线路表：线路名 → 代理地址（http / https / socks5 / socks5h）。
	//
	// 面板会给账号绑一条线路（auths/*.json 顶层 `proxy` 写的是**线路名**），这个名字
	// 在这里解析成地址；线路名查不到时上游**明确报错、不静默直连**（静默直连等于
	// 「以为隔离了其实没隔离」，见 upstream.TransportFor）。
	Proxies map[string]string `json:"proxies"`

	// ReasoningAsContent 思考通道兜底（issue #135）：国际版 deepseek 思考模式下
	// 答案只出现在 reasoning_content、content 恒空，只看 content 的客户端会看到
	// 空回复。开启后，**整轮既无正文也无工具调用**时把思考原文同时作为 content
	// 下发；正常轮次零改动。
	//
	// 默认 true（空回复比重复更难接受）。代价：同时渲染两个字段的客户端会把同一
	// 段话显示两遍 —— 遇到就设 `"reasoning_as_content": false`，或环境变量
	// WB2A_REASONING_AS_CONTENT=0。指针类型用于区分「没写」与「显式 false」。
	ReasoningAsContent *bool `json:"reasoning_as_content"`

	Server struct{} `json:"server"` // 已退役段：max_body_mb 移除后无字段；旧配置该段下任意键因 JSON 未知字段而自然忽略

	Cooldown struct {
		// hard_credit / err_threshold / err_cooldown 三个历史键已退役：
		// 硬冷却固定为次日 04:00（CooldownUntilTomorrow4AM），连续错误语义并入熔断器。
		// 旧 config 中的这些键因 JSON 未知字段而自然忽略，不报错。
		SoftRate string `json:"soft_rate"` // "600s"，软限流冷却基数
		// SoftRateMax 软冷却指数退避的封顶，默认 "2h"。
		// 空值回落默认，非法值报错（处理风格同 soft_rate）。
		SoftRateMax string `json:"soft_rate_max"` // "2h"
		// CreditExhausted 积分耗尽后的冷却时长（如 "10m"）。空 = 旧行为：硬冷却到次日 04:00
		// （等签到恢复）。配了就只退出这一小段，到期回到候选池（issue #78）。
		CreditExhausted string `json:"credit_exhausted"`
		// NotFound / WafBase / UpstreamRestriction 各类冷却时长（默认值见 config.default.json）。
		NotFound            string `json:"not_found"`            // 404/未知模型后的冷却，默认 "60s"
		WafBase             string `json:"waf_base"`             // WAF 403 基础冷却，默认 "60s"
		UpstreamRestriction string `json:"upstream_restriction"` // 上游限制类错误冷却，默认 "45m"
	} `json:"cooldown"`

	Schedule config.Schedule `json:"schedule"`

	// Admin 运维管理端点开关（issue #138/#118）。默认**关闭**：管理能力默认不暴露，
	// 避免「开了网关就等于开了账号管理面」。开启后
	// /admin/accounts/{uid}/{disable,enable,revive} 可用；鉴权与 /status 同源
	// （withAuth + 同一个 api_key，不另立管理密钥）。
	Admin struct {
		Enabled bool `json:"enabled"` // 默认 false
		// BodyLimitBytes 管理面请求体上限（默认 8192）。
		BodyLimitBytes int `json:"body_limit_bytes"`
		// PasswordLength 启动时随机生成的管理密码长度（默认 20）。
		PasswordLength int `json:"password_length"`
		// MinPasswordLength 用 WB2A_ADMIN_PASSWORD 固定密码时的最小长度（默认 8）。
		MinPasswordLength int `json:"min_password_length"`
	} `json:"admin"`

	Global struct {
		// Enabled global realm 路由开关。缺省 true：Realm() 正常把 realm=global/
		// domain=workbuddy.ai 的账号判为 global 并路由 global base/路径。
		// 显式 "enabled": false 关闭（逃生门，纯 CN 锁定：即便 auth 写了 realm=global
		// 也不路由，auth.Realm() 双保险的第一道闸）。纯 CN 部署行为不变：CN 账号
		// 恒判 cn，global base 只在 realm=global 的账号上被使用。
		Enabled bool `json:"enabled"`
		// ChatBase / BillingBase 国际版上游 base 覆盖；空 = 回落内置默认
		// https://www.workbuddy.ai（D5，internal/upstream.defaultGlobalBase）。
		ChatBase    string `json:"chat_base"`
		BillingBase string `json:"billing_base"`
	} `json:"global"`

	Upstream struct {
		// TimeoutSeconds 短 RPC（refresh/checkin/balance/FetchModels）总时长上限，默认 120。
		TimeoutSeconds int `json:"timeout_seconds"`
		// HeaderTimeoutSeconds 聊天 SSE 首字节前（响应头）上限；<=0 回落 TimeoutSeconds。
		HeaderTimeoutSeconds int `json:"header_timeout_seconds"`
		// IdleTimeoutSeconds 聊天 SSE 流中空闲上限（活跃吐数据续命不掐）；<=0 回落默认 300。
		IdleTimeoutSeconds int `json:"idle_timeout_seconds"`
		// UserAgent 出站 User-Agent 显式覆盖（非空时全路径生效，优先于默认三段式）。
		// 全部出站请求生效：chat/refresh/checkin/balance/report/travel/FetchModels。
		// issue #42 深挖：官网「使用端」列基于出站请求 UA 的服务端归因，官方 WorkBuddy
		// 桌面 UA 为 `WorkBuddy/<version>`。默认值已对齐官方（A 段变更），用户仍可配完全
		// 自定义值改写。
		UserAgent string `json:"user_agent"`
		// ClientVersion WorkBuddy 客户端版本段（出站 UA 的 `WorkBuddy/<ver>` 与白名单
		// 头组 X-IDE-Version 的取值）。空 = 内置默认（对齐官方 5.5.4 分发包）；
		// 显式配置（如升级后的桌面包版本）则随配置走。
		ClientVersion string `json:"client_version"`
		// CliVersion 出站 UA 中 `CLI/<ver>` 段的版本。空 = 内置默认（对齐官方内置 CLI
		// 2.137.1）；显式配置则随配置走。
		CliVersion string `json:"cli_version"`

		// DeviceToken 设备风控 Token（X-Device-Token 头）全局兜底。
		// 容器内无桌面端 Turing SDK，这是把外部生成的 token 注入的入口；空 = 不注入。
		// 每号覆盖优先级：auths 文件 device_token > 本全局值 > DeviceTokenFile（文件兜底）。
		DeviceToken string `json:"device_token"`
		// DeviceTokenFile 宿主落盘的 device token 文件路径（可选，空 = 不读文件）。
		// 读取频率限 5 分钟一次缓存，>1KB 或读失败则忽略（优雅降级不注入）。
		DeviceTokenFile string `json:"device_token_file"`
		// RetryAfterMax 上游 Retry-After 的合理上限（超出视为脏数据），默认 "2h"。
		RetryAfterMax string `json:"retry_after_max"`
		// GzipThresholdBytes 请求体超过该字节数才压缩（0 = 不压缩），默认 65536。
		GzipThresholdBytes int `json:"gzip_threshold_bytes"`
		// RefreshIOTimeout token 刷新的单独超时，默认 "30s"。
		RefreshIOTimeout string `json:"refresh_io_timeout"`
		// DeviceTokenTTL / DeviceTokenMaxLen device_token 文件缓存有效期与最大长度。
		DeviceTokenTTL    string `json:"device_token_ttl"`     // 默认 "5m"
		DeviceTokenMaxLen int    `json:"device_token_max_len"` // 默认 1024
		// ClientName 用量归属头 X-Product/X-IDE-Name/X-IDE-Type 的取值。
		// 空（缺省）= "WorkBuddy"：伪造官方桌面端指纹（X-IDE-* 四头 + X-Agent-Purpose，
		// 上游用量归因不再出现 client/agentPurpose 为空的网关特征）。
		// 显式配 "SaaS" 还原旧行为（仅 X-Product="SaaS"，不设 X-IDE-*）。
		ClientName string `json:"client_name"`
		// PassthroughIP 是否透传客户端 IP（X-Forwarded-For/X-Real-IP 首段）给上游。
		// 缺省 false（反代安全边界：不把内网/代理 IP 暴露给上游）；true 才透传。
		PassthroughIP bool `json:"passthrough_ip"`
		// GzipBody chat 请求体超过阈值时用 gzip 压缩后再发上游（issue #297）。
		// 缺省 true：服务器出口带宽窄时（实测 1Mbps）3MB 上下文可省 20s+ 首字节；
		// 小于阈值（64KB）的请求不压缩，短对话零开销。排障可显式配 false。
		GzipBody bool `json:"gzip_body"`
	} `json:"upstream"`

	Features struct {
		// SanitizeBlacklistFingerprints 出站请求体黑名单指纹脱敏（默认 true；false 完全还原）。
		SanitizeBlacklistFingerprints bool `json:"sanitize_blacklist_fingerprints"`
		// SanitizeExtraFeatures 运行期追加的特征预检串：任一命中即进入净化。
		// 上游黑名单是逐词扩充的（GUI 句 → workspace root → 后续还会加），
		// 放配置里就不必为每个新词重编译镜像；空 = 只用内置静态表。
		SanitizeExtraFeatures []string `json:"sanitize_extra_features"`
		// SanitizeExtraRewrites 运行期追加的逐字替换对，格式 [["原串","替换串"], …]。
		// 只做精确替换（大小写敏感），与 sanitize_extra_features 配套使用：
		// 通常把新增词先加进 features 保证进入净化分支，再加进 rewrites 做实际改写。
		SanitizeExtraRewrites [][2]string `json:"sanitize_extra_rewrites"`
	} `json:"features"`

	Prompt struct {
		// Mode passthrough（默认）= 透传客户端原始 system（降级重试仍会切到 Degraded）；
		// custom = 网关用自有系统提示词替换客户端 system/developer（显式配置仍可覆盖回替换）；
		// append = 两者并用：开头连续 system/developer 块后插网关 system，既有消息逐字不动（issue #129）。
		Mode string `json:"mode"` // "custom" / "append" / "passthrough"
		// File 提示词文件路径；空 = 内置默认 defaultprompt.md；
		// 路径非空但不可读 → 启动报错（fail fast，避免静默回落到内置默认）。
		File string `json:"file"`
	} `json:"prompt"`

	// PromptText 解析后的系统提示词文本（custom/append 模式使用）。
	PromptText string `json:"-"`

	Upstash struct {
		URL   string `json:"url"`   // 空 = 纯内存模式；支持完整 rediss:// URL 或 https://xxx.upstash.io host
		Token string `json:"token"` // url 非完整连接串时用于组装 rediss://default:<token>@<host>:6379
		// KeyTTL 会话镜像 key 的存活时间（默认 "168h"）；WriteConcurrency 并发写入上限（默认 8）。
		KeyTTL           string `json:"key_ttl"`
		WriteConcurrency int    `json:"write_concurrency"`
	} `json:"upstash"`

	Pool struct {
		// 大上下文请求的逐请求轮换（本地增强，默认关闭）：只有
		// large_request_cooldown > 0 时才生效。请求体 ≥ large_request_bytes 时放弃
		// 会话粘性、按请求换号，并在窗口内不再复用同号。前缀缓存按账号存放，轮换会让
		// 长上下文的缓存整段失效，所以默认 "0s"（关闭），风控现场再显式打开。
		LargeRequestBytes    int    `json:"large_request_bytes"`    // 默认 2097152（2 MiB）；<=0 关闭
		LargeRequestCooldown string `json:"large_request_cooldown"` // 默认 "0s"（关闭）；>0 才启用轮换
		// 大请求预算（推荐策略）：滑动窗口内某号承接的大请求达到该条数后，下一条大请求
		// 不再给它（会话迁移到别的号），窗口滑过后自动恢复。默认 6；<=0 关闭预算机制。
		// 与 large_request_cooldown 互斥：预算 > 0 时走预算，否则按 cooldown 走逐请求轮换。
		LargeRequestBudget int `json:"large_request_budget"`
		// LargeRequestBudgetWindow 预算的滑动窗口，默认 "120s"。
		LargeRequestBudgetWindow string `json:"large_request_budget_window"`
		CreditFloor              int64  `json:"credit_floor"`         // 保底积分（issue #364）：低于该值的账号只作最后候选，0 = 关闭
		MaxInFlight              int    `json:"max_in_flight"`        // 单账号最大在途请求数，0 = 不限
		MaxInFlightGlobal        int    `json:"max_in_flight_global"` // global 域单账号在途上限（WAF 403 风控分档），0 = 回落 max_in_flight
		BreakerThreshold         int    `json:"breaker_threshold"`    // 连续失败次数触发熔断，默认 3
		BreakerCooldown          string `json:"breaker_cooldown"`     // 基础熔断时长，默认 "30m"
		BreakerCooldownMax       string `json:"breaker_cooldown_max"` // 指数退避封顶，默认 "6h"
		// 连败降权（issue #114「累计错误率高/连续失败 N 次的账号移出候选池一段时间」）：
		// ErrClient/传输层这类「不罚号」失败连续计数，达阈临时出池。与冷却/熔断
		// 并存取更长者不叠加。默认 5 次 / 10m。
		DegradeThreshold   int     `json:"degrade_threshold"`    // 连败次数触发降权，默认 5
		DegradeCooldown    string  `json:"degrade_cooldown"`     // 降权时长（固定，非指数退避），默认 "10m"
		DegradeCooldownMax string  `json:"degrade_cooldown_max"` // 降权时长的上限钳制，默认 "2h"（仅当 cooldown 超该值才钳制）
		IdleWeightPerHour  float64 `json:"idle_weight_per_hour"` // 闲置补偿：每小时未用 +0.5 权重
		IdleWeightMax      float64 `json:"idle_weight_max"`      // 闲置补偿封顶，默认 5.0
		// ExpiringSoon 快过期积分窗口（如 "168h"=7天）：签到查余额时，到期时间在此窗口内
		// 的积分被标记为"快过期"，选号优先消耗（issue:积分过期）。空/0 = 禁用分桶。
		ExpiringSoon string `json:"expiring_soon"`
		// CostExploreInterval costTier 条件探索窗口（issue #136 方案 a′）：tier 0
		// 垄断层存在且 tier 1 有成员时，距上次探索 ≥ 窗口则本次 pick 生效层切
		// tier 1-only（探索=搭车改道，零新增上游请求；成功即毕业，失败走既有
		// 错误策略）。默认 "30m"（≤48 次/天/模型）；"0" 关停（完全回到现状行为）；
		// 空值回落默认。
		CostExploreInterval string `json:"cost_explore_interval"`
		// MinPickGap 选号防惊群间隔（跳过刚被选中的号），默认 "100ms"。
		MinPickGap string `json:"min_pick_gap"`
		// SessionDeadThreshold 连续多少次 12153 session dead 才禁用账号，默认 3。
		SessionDeadThreshold int `json:"session_dead_threshold"`
		// RotateBackoffBase 转退避基数（对齐官方 CLI），默认 "500ms"。
		RotateBackoffBase string `json:"rotate_backoff_base"`
		// ExpiringWeight 快过期积分在选号权重里的倍数，默认 8。
		ExpiringWeight float64 `json:"expiring_weight"`
	} `json:"pool"`

	// Auths auths 目录热加载（默认 "5s"）。
	Auths struct {
		WatchInterval string `json:"watch_interval"`
	} `json:"auths"`

	// State 状态落盘（默认 "5s"）。
	State struct {
		FlushInterval string `json:"flush_interval"`
	} `json:"state"`

	// Risk 风控阈值：单 IP 在窗口内命中 WAF 多少次进入风险暂停。
	Risk struct {
		WafWindow    string `json:"waf_window"`    // 默认 "60s"
		WafThreshold int    `json:"waf_threshold"` // 默认 2
	} `json:"risk"`

	// Metrics 指标环形缓冲容量（默认 512）。
	Metrics struct {
		Capacity int `json:"capacity"`
	} `json:"metrics"`

	// Debug 排障开关：请求体落盘的最小字节数（0 = 关闭，默认 4194304）。
	Debug struct {
		DumpMinBytes int `json:"dump_min_bytes"`
	} `json:"debug"`

	// Models 模型目录与上下文窗口默认值。
	Models struct {
		DefaultContext         int64  `json:"default_context"`          // 默认 1000000
		CatalogTimeout         string `json:"catalog_timeout"`          // 默认 "5s"
		CatalogRefreshCooldown string `json:"catalog_refresh_cooldown"` // 默认 "5m"
		CatalogNegativeTTL     string `json:"catalog_negative_ttl"`     // 默认 "24h"
	} `json:"models"`

	SessionSticky struct {
		Enabled    bool   `json:"enabled"`     // 默认 true
		TTL        string `json:"ttl"`         // 会话绑定 TTL，默认 "30m"
		GCInterval string `json:"gc_interval"` // 会话 GC 周期，默认 "5m"
	} `json:"session_sticky"`

	// 解析后
	SoftRateDur                 time.Duration `json:"-"`
	SoftRateMaxDur              time.Duration `json:"-"`
	CreditExhaustedDur          time.Duration `json:"-"`
	BreakerCooldownDur          time.Duration `json:"-"`
	BreakerCooldownMaxD         time.Duration `json:"-"`
	DegradeCooldownDur          time.Duration `json:"-"`
	DegradeCooldownMaxD         time.Duration `json:"-"`
	SessionTTL                  time.Duration `json:"-"`
	LargeRequestCooldownDur     time.Duration `json:"-"`
	LargeRequestBudgetWindowDur time.Duration `json:"-"`
	SessionGCInterval           time.Duration `json:"-"`
	ExpiringSoonDur             time.Duration `json:"-"`
	// CostExploreIntervalDur 解析后的 costTier 探索窗口（issue #136）；0 = 关停。
	CostExploreIntervalDur time.Duration `json:"-"`
	// SpreadWindowDur 定时任务账号随机分布窗口（schedule.spread_window 解析结果）。
	// 0 = 关闭（回到"到点立刻 + 固定间隔"）。默认 15m。
	SpreadWindowDur time.Duration `json:"-"`
	// SourcePath 本次配置的实际来源（config.json / 内置默认 builtin:config.default.json）。
	// 只用于启动日志与排障，不参与 JSON 序列化。
	SourcePath string `json:"-"`
	// IntraJitterMinD/MaxD 同账号内连续请求的随机间隔区间
	// （schedule.intra_request_jitter_min/max 解析结果）。默认 1s / 3s；都 0 = 关闭。
	IntraJitterMinD time.Duration `json:"-"`
	IntraJitterMaxD time.Duration `json:"-"`

	// A 组运行期阈值的解析结果（json:"-"）；解析后注入各包的 Configure。
	CooldownNotFoundDur            time.Duration `json:"-"`
	CooldownWafBaseDur             time.Duration `json:"-"`
	CooldownUpstreamRestrictionDur time.Duration `json:"-"`
	MinPickGapDur                  time.Duration `json:"-"`
	RotateBackoffBaseDur           time.Duration `json:"-"`
	RetryAfterMaxDur               time.Duration `json:"-"`
	RefreshIOTimeoutDur            time.Duration `json:"-"`
	DeviceTokenTTLDur              time.Duration `json:"-"`
	AuthsWatchIntervalDur          time.Duration `json:"-"`
	StateFlushIntervalDur          time.Duration `json:"-"`
	RiskWafWindowDur               time.Duration `json:"-"`
	ModelsCatalogTimeoutDur        time.Duration `json:"-"`
	ModelsCatalogRefreshDur        time.Duration `json:"-"`
	ModelsCatalogNegativeDur       time.Duration `json:"-"`
	UpstashKeyTTLDur               time.Duration `json:"-"`
}

// Default 默认配置 —— **完全来自内嵌的 config.default.json**（唯一权威来源）。
//
// 为什么这样做：默认值以前硬编码在这个函数里（listen、超时、池阈值、排程时刻……几十个），
// 用户读 README 也拼不出"完整默认配置"，改默认值要翻 Go 代码。现在唯一的权威来源是
// internal/configdata/config.default.json（随二进制内嵌）：
//   - 想看/想改默认值 → 改那份 JSON（根目录的 config.default.json 是它的对外副本，
//     用 `go run ./cmd/config-default -write` 同步，测试会强制两者一致）；
//   - config.json 缺失时，Load 也回退到这份示例（见 internal/configdata.Resolve）。
//
// 需要指针语义的少数字段（如 ReasoningAsContent）由 JSON 里的 true/false 直接表达。
func Default() *Config {
	c := &Config{}
	if err := json.Unmarshal(configdata.Bytes(), c); err != nil {
		// 内嵌文件在编译期就固定了：解析失败只能是构建问题，带病启动更难查。
		panic("内置 config.default.json 无法解析: " + err.Error())
	}
	return c
}

// Load 读取配置，语义与 Spring 的属性文件一致：
//
//	配置 = 内置默认配置(config.default.json) ← 被 config.json 逐属性覆盖
//
// 也就是说：
//   - config.json **文件缺失**（或该路径其实是个目录）→ 全部属性取内置默认值，并打 WARNING；
//   - config.json 存在但**某个属性没写** → 该属性取 config.default.json 里的值；
//   - 属性**写成 null** 也按"未定义"处理（回退到默认值），而不是清成零值——
//     JSON 里 `"ttl": null` 的直觉含义就是"没设"；
//   - 最终再用 WB2A_* 环境变量覆盖（最高优先级）。
func Load(path string) (*Config, error) {
	c := Default()

	raw, source, err := configdata.Resolve(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	c.SourcePath = source

	// 容忍 UTF-8 BOM：Windows 记事本 / PowerShell 5.1 的 `Set-Content -Encoding utf8`
	// 默认会写 BOM，而 encoding/json 遇到 \ufeff 会直接报
	// 「invalid character 'ï' looking for beginning of value」——
	// 用户看到的是一句和配置文件内容毫不相干的解析错误。
	raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})
	// null = 未定义：先剔除所有 null 值，让默认配置的值留下来。
	raw, err = stripNulls(raw)
	if err != nil {
		return nil, fmt.Errorf("parse config %s: %w", source, err)
	}
	if err := json.Unmarshal(raw, c); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", source, err)
	}
	applyEnv(c)
	if err := c.normalize(); err != nil {
		return nil, err
	}
	if err := c.wireTunings(); err != nil {
		return nil, err
	}
	return c, nil
}

// wireTunings 解析 A 组运行期阈值并注入各包（默认值全部来自 config.default.json）。
//
// 为什么单独一步：这些阈值是**进程级**参数，散在 pool/server/upstream/redisstore/adminauth
// 五个包里；统一在这里解析+注入，配置面（JSON 键）与实现面（包级变量）就能一一对上，
// 也便于测试断言"默认值确实来自默认配置文件"。
func (c *Config) wireTunings() error {
	parseDur := func(key, raw string) (time.Duration, error) {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return 0, nil
		}
		d, err := time.ParseDuration(raw)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", key, err)
		}
		if d < 0 {
			return 0, fmt.Errorf("%s: 不能为负（关闭请写 0）", key)
		}
		return d, nil
	}
	items := []struct {
		key string
		raw string
		dst *time.Duration
	}{
		{"cooldown.not_found", c.Cooldown.NotFound, &c.CooldownNotFoundDur},
		{"cooldown.waf_base", c.Cooldown.WafBase, &c.CooldownWafBaseDur},
		{"cooldown.upstream_restriction", c.Cooldown.UpstreamRestriction, &c.CooldownUpstreamRestrictionDur},
		{"pool.min_pick_gap", c.Pool.MinPickGap, &c.MinPickGapDur},
		{"pool.rotate_backoff_base", c.Pool.RotateBackoffBase, &c.RotateBackoffBaseDur},
		{"upstream.retry_after_max", c.Upstream.RetryAfterMax, &c.RetryAfterMaxDur},
		{"upstream.refresh_io_timeout", c.Upstream.RefreshIOTimeout, &c.RefreshIOTimeoutDur},
		{"upstream.device_token_ttl", c.Upstream.DeviceTokenTTL, &c.DeviceTokenTTLDur},
		{"auths.watch_interval", c.Auths.WatchInterval, &c.AuthsWatchIntervalDur},
		{"state.flush_interval", c.State.FlushInterval, &c.StateFlushIntervalDur},
		{"risk.waf_window", c.Risk.WafWindow, &c.RiskWafWindowDur},
		{"models.catalog_timeout", c.Models.CatalogTimeout, &c.ModelsCatalogTimeoutDur},
		{"models.catalog_refresh_cooldown", c.Models.CatalogRefreshCooldown, &c.ModelsCatalogRefreshDur},
		{"models.catalog_negative_ttl", c.Models.CatalogNegativeTTL, &c.ModelsCatalogNegativeDur},
		{"upstash.key_ttl", c.Upstash.KeyTTL, &c.UpstashKeyTTLDur},
	}
	for _, it := range items {
		d, err := parseDur(it.key, it.raw)
		if err != nil {
			return err
		}
		*it.dst = d
	}

	adminauth.Configure(adminauth.Options{
		PasswordLength:    c.Admin.PasswordLength,
		MinOverrideLength: c.Admin.MinPasswordLength,
	})
	pool.Configure(pool.Options{
		MinPickGap:           c.MinPickGapDur,
		SessionDeadThreshold: c.Pool.SessionDeadThreshold,
		ExpiringWeight:       c.Pool.ExpiringWeight,
		WatchInterval:        c.AuthsWatchIntervalDur,
		FlushInterval:        c.StateFlushIntervalDur,
	})
	server.Configure(server.Options{
		NotFoundCooldown:            c.CooldownNotFoundDur,
		WafCooldownBase:             c.CooldownWafBaseDur,
		UpstreamRestrictionCooldown: c.CooldownUpstreamRestrictionDur,
		RotateBackoffBase:           c.RotateBackoffBaseDur,
		WafIPWindow:                 c.RiskWafWindowDur,
		WafIPThreshold:              c.Risk.WafThreshold,
		DumpReqMinBytes:             c.Debug.DumpMinBytes,
		MetricsCap:                  c.Metrics.Capacity,
		AdminBodyLimit:              int64(c.Admin.BodyLimitBytes),
	})
	upstream.Configure(upstream.Options{
		RetryAfterMax:          c.RetryAfterMaxDur,
		RefreshIOTimeout:       c.RefreshIOTimeoutDur,
		GzipThresholdBytes:     c.Upstream.GzipThresholdBytes,
		DeviceTokenFileTTL:     c.DeviceTokenTTLDur,
		DeviceTokenFileMaxLen:  c.Upstream.DeviceTokenMaxLen,
		DefaultContextWindow:   c.Models.DefaultContext,
		CatalogTimeout:         c.ModelsCatalogTimeoutDur,
		CatalogRefreshCooldown: c.ModelsCatalogRefreshDur,
		CatalogNegativeTTL:     c.ModelsCatalogNegativeDur,
	})
	redisstore.Configure(redisstore.Options{
		KeyTTL:           c.UpstashKeyTTLDur,
		WriteConcurrency: c.Upstash.WriteConcurrency,
	})
	return nil
}

// stripNulls 递归剔除 JSON 对象里值为 null 的键，让"显式 null"等同于"未定义"，
// 从而回退到默认配置的值（Spring 式属性覆盖语义）。
//
// 为什么不用 map[string]json.RawMessage 手工合并：那要为每个嵌套段写一遍合并逻辑；
// 这里先解码成通用结构、删掉 null、再编码回字节，交给随后的 Unmarshal 统一处理，
// 对以后新增的配置段天然生效。
func stripNulls(raw []byte) ([]byte, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	out, err := json.Marshal(dropNulls(v))
	if err != nil {
		return nil, err
	}
	return out, nil
}

func dropNulls(v any) any {
	switch val := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(val))
		for k, item := range val {
			if item == nil {
				continue
			}
			out[k] = dropNulls(item)
		}
		return out
	case []any:
		out := make([]any, len(val))
		for i, item := range val {
			out[i] = dropNulls(item)
		}
		return out
	default:
		return v
	}
}

// defaults 返回「纯默认配置」（供 normalize 做"显式空值 → 默认值"的兜底）。
//
// 兜底值同样取自 config.default.json，不放字面量：这样"默认值只有一处来源"始终成立。
func defaults() *Config {
	defaultCfgOnce.Do(func() {
		defaultCfg = Default()
	})
	return defaultCfg
}

func applyEnv(c *Config) {
	if v := os.Getenv("WB2A_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("WB2A_API_KEY"); v != "" {
		c.APIKey = v
	}
	if v := os.Getenv("WB2A_AUTH_DIR"); v != "" {
		c.AuthDir = v
	}
	if v := os.Getenv("WB2A_STATE_FILE"); v != "" {
		c.StateFile = v
	}
	if v := os.Getenv("WB2A_SOFT_RATE"); v != "" {
		c.Cooldown.SoftRate = v
	}
	if v := os.Getenv("WB2A_SOFT_RATE_MAX"); v != "" {
		c.Cooldown.SoftRateMax = v
	}
	if v := os.Getenv("WB2A_CREDIT_EXHAUSTED"); v != "" {
		c.Cooldown.CreditExhausted = v
	}
	if v := strings.TrimSpace(os.Getenv("WB2A_REASONING_AS_CONTENT")); v != "" {
		on := !(v == "0" || strings.EqualFold(v, "false") || strings.EqualFold(v, "off"))
		c.ReasoningAsContent = &on
	}
	if v := strings.TrimSpace(os.Getenv("WB2A_PROXIES")); v != "" {
		var table map[string]string
		if err := json.Unmarshal([]byte(v), &table); err == nil {
			c.Proxies = table
		}
	}
	if v := os.Getenv("WB2A_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.TimeoutSeconds = n
		}
	}
	if v := os.Getenv("WB2A_HEADER_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.HeaderTimeoutSeconds = n
		}
	}
	if v := os.Getenv("WB2A_IDLE_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.IdleTimeoutSeconds = n
		}
	}
	if v := os.Getenv("WB2A_USER_AGENT"); v != "" {
		c.Upstream.UserAgent = v
	}
	if v := os.Getenv("WB2A_DEVICE_TOKEN"); v != "" {
		c.Upstream.DeviceToken = v
	}
	if v := os.Getenv("WB2A_DEVICE_TOKEN_FILE"); v != "" {
		c.Upstream.DeviceTokenFile = v
	}
	if v := os.Getenv("WB2A_CLIENT_NAME"); v != "" {
		c.Upstream.ClientName = v
	}
	if v := os.Getenv("WB2A_CLIENT_VERSION"); v != "" {
		c.Upstream.ClientVersion = v
	}
	if v := os.Getenv("WB2A_CLI_VERSION"); v != "" {
		c.Upstream.CliVersion = v
	}
	if v := os.Getenv("WB2A_PASSTHROUGH_IP"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Upstream.PassthroughIP = b
		}
	}
	if v := os.Getenv("WB2A_GZIP_BODY"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Upstream.GzipBody = b
		}
	}
	if v := os.Getenv("WB2A_SANITIZE_FINGERPRINTS"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Features.SanitizeBlacklistFingerprints = b
		}
	}
	if v := os.Getenv("WB2A_PROMPT_MODE"); v != "" {
		c.Prompt.Mode = v
	}
	if v := os.Getenv("WB2A_PROMPT_FILE"); v != "" {
		c.Prompt.File = v
	}
	if v := os.Getenv("WB2A_EXPIRING_SOON"); v != "" {
		c.Pool.ExpiringSoon = v
	}
	if v := os.Getenv("WB2A_ADMIN_ENABLED"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Admin.Enabled = b
		}
	}
}

func (c *Config) normalize() error {
	var err error
	if c.SoftRateDur, err = time.ParseDuration(c.Cooldown.SoftRate); err != nil {
		return fmt.Errorf("cooldown.soft_rate: %w", err)
	}
	// 空值回落默认 2h（Default() 已置值；此兜底覆盖显式 "" 与 Default() 被绕过的场景）。
	if c.Cooldown.SoftRateMax == "" {
		c.Cooldown.SoftRateMax = defaults().Cooldown.SoftRateMax
	}
	if c.SoftRateMaxDur, err = time.ParseDuration(c.Cooldown.SoftRateMax); err != nil {
		return fmt.Errorf("cooldown.soft_rate_max: %w", err)
	}
	// 出口线路表：名字与地址都去空白；scheme 只认 http/https/socks5/socks5h。
	// 这里**启动即校验**：地址写错时让上游起不来（带明确提示），比跑起来以后每个
	// 请求都失败容易发现得多；线路名查不到（账号引用了不存在的名字）则在用时明确报错。
	if c.ReasoningAsContent == nil {
		on := true
		c.ReasoningAsContent = &on
	}
	if len(c.Proxies) > 0 {
		clean := make(map[string]string, len(c.Proxies))
		for name, addr := range c.Proxies {
			n := strings.TrimSpace(name)
			v := strings.TrimSpace(addr)
			if n == "" || v == "" {
				continue
			}
			u, err := url.Parse(v)
			if err != nil {
				return fmt.Errorf("proxies[%s]: %w", n, err)
			}
			switch strings.ToLower(u.Scheme) {
			case "http", "https", "socks5", "socks5h":
			default:
				return fmt.Errorf("proxies[%s]: 不支持的协议 %q（只能是 http/https/socks5/socks5h）", n, u.Scheme)
			}
			if u.Host == "" {
				return fmt.Errorf("proxies[%s]: 地址缺少主机名（%q）", n, v)
			}
			clean[n] = v
		}
		c.Proxies = clean
	}

	// 积分耗尽后的冷却时长（issue #78）：空 = 保持「硬冷却到次日 04:00」的旧行为；
	// 填了必须是正时长（负值会让冷却立即到期，等于没冷却 —— 与其静默放行，不如报错）。
	if v := strings.TrimSpace(c.Cooldown.CreditExhausted); v == "" {
		c.CreditExhaustedDur = 0
	} else {
		d, perr := time.ParseDuration(v)
		if perr != nil {
			return fmt.Errorf("cooldown.credit_exhausted: %w", perr)
		}
		if d <= 0 {
			return fmt.Errorf("cooldown.credit_exhausted: must be a positive duration, got %q", v)
		}
		c.CreditExhaustedDur = d
	}
	if c.BreakerCooldownDur, err = time.ParseDuration(c.Pool.BreakerCooldown); err != nil {
		return fmt.Errorf("pool.breaker_cooldown: %w", err)
	}
	if c.BreakerCooldownMaxD, err = time.ParseDuration(c.Pool.BreakerCooldownMax); err != nil {
		return fmt.Errorf("pool.breaker_cooldown_max: %w", err)
	}
	if c.DegradeCooldownDur, err = time.ParseDuration(c.Pool.DegradeCooldown); err != nil {
		return fmt.Errorf("pool.degrade_cooldown: %w", err)
	}
	if c.DegradeCooldownMaxD, err = time.ParseDuration(c.Pool.DegradeCooldownMax); err != nil {
		return fmt.Errorf("pool.degrade_cooldown_max: %w", err)
	}
	if c.Pool.LargeRequestCooldown == "" {
		c.Pool.LargeRequestCooldown = defaults().Pool.LargeRequestCooldown
	}
	if c.LargeRequestCooldownDur, err = time.ParseDuration(c.Pool.LargeRequestCooldown); err != nil {
		return fmt.Errorf("pool.large_request_cooldown: %w", err)
	}
	if c.Pool.LargeRequestBudgetWindow == "" {
		c.Pool.LargeRequestBudgetWindow = defaults().Pool.LargeRequestBudgetWindow
	}
	if c.LargeRequestBudgetWindowDur, err = time.ParseDuration(c.Pool.LargeRequestBudgetWindow); err != nil {
		return fmt.Errorf("pool.large_request_budget_window: %w", err)
	}
	if c.Pool.LargeRequestBudget == 0 {
		c.Pool.LargeRequestBudget = defaults().Pool.LargeRequestBudget
	}
	if c.Pool.LargeRequestBytes == 0 {
		c.Pool.LargeRequestBytes = defaults().Pool.LargeRequestBytes
	}
	if c.Pool.CreditFloor < 0 {
		c.Pool.CreditFloor = 0
	}
	if c.SessionTTL, err = time.ParseDuration(c.SessionSticky.TTL); err != nil {
		return fmt.Errorf("session_sticky.ttl: %w", err)
	}
	if c.SessionGCInterval, err = time.ParseDuration(c.SessionSticky.GCInterval); err != nil {
		return fmt.Errorf("session_sticky.gc_interval: %w", err)
	}
	if c.Pool.BreakerThreshold <= 0 {
		c.Pool.BreakerThreshold = 3
	}
	// 连败降权参数缺省归一（非法/未设置回落默认，与 breaker_threshold 同风格）。
	if c.Pool.DegradeThreshold <= 0 {
		c.Pool.DegradeThreshold = 5
	}
	if c.Pool.DegradeCooldown == "" {
		c.Pool.DegradeCooldown = defaults().Pool.DegradeCooldown
	}
	if c.Pool.DegradeCooldownMax == "" {
		c.Pool.DegradeCooldownMax = defaults().Pool.DegradeCooldownMax
	}
	// global 在途分档：0/负数视为未设置回落默认 2（WAF 403 修复 P1-1）。
	if c.Pool.MaxInFlightGlobal <= 0 {
		c.Pool.MaxInFlightGlobal = defaults().Pool.MaxInFlightGlobal
	}
	if c.Pool.IdleWeightPerHour <= 0 {
		c.Pool.IdleWeightPerHour = 0.5
	}
	if c.Pool.IdleWeightMax <= 0 {
		c.Pool.IdleWeightMax = 5.0
	}
	// 定时任务账号随机分布窗口（schedule.spread_window）：
	// 空值回落默认 15m；"0" 是合法值（关闭，回到固定间隔）；负值钳 0。
	if c.Schedule.SpreadWindow == "" {
		c.Schedule.SpreadWindow = defaults().Schedule.SpreadWindow
	}
	if c.SpreadWindowDur, err = time.ParseDuration(c.Schedule.SpreadWindow); err != nil {
		return fmt.Errorf("schedule.spread_window: %w", err)
	}
	if c.SpreadWindowDur < 0 {
		c.SpreadWindowDur = 0
	}
	// 同账号内连续请求的随机间隔（schedule.intra_request_jitter_min/max）：
	// 空值回落默认 1s / 3s；"0" 是合法值（关闭）；min>max 视为配置错误（早报错早发现）。
	if c.Schedule.IntraRequestJitterMin == "" {
		c.Schedule.IntraRequestJitterMin = defaults().Schedule.IntraRequestJitterMin
	}
	if c.Schedule.IntraRequestJitterMax == "" {
		c.Schedule.IntraRequestJitterMax = defaults().Schedule.IntraRequestJitterMax
	}
	if c.IntraJitterMinD, err = time.ParseDuration(c.Schedule.IntraRequestJitterMin); err != nil {
		return fmt.Errorf("schedule.intra_request_jitter_min: %w", err)
	}
	if c.IntraJitterMaxD, err = time.ParseDuration(c.Schedule.IntraRequestJitterMax); err != nil {
		return fmt.Errorf("schedule.intra_request_jitter_max: %w", err)
	}
	if c.IntraJitterMinD < 0 || c.IntraJitterMaxD < 0 {
		return fmt.Errorf("schedule.intra_request_jitter_*: 不能为负（关闭请写 0）")
	}
	if c.IntraJitterMaxD < c.IntraJitterMinD {
		return fmt.Errorf("schedule.intra_request_jitter_max(%s) 不能小于 min(%s)",
			c.IntraJitterMaxD, c.IntraJitterMinD)
	}
	// 快过期窗口：空值回落默认 168h（Default 已置；此兜底覆盖显式 ""）；显式 "0"/负值 = 禁用分桶。
	if c.Pool.ExpiringSoon == "" {
		c.Pool.ExpiringSoon = defaults().Pool.ExpiringSoon
	}
	if c.ExpiringSoonDur, err = time.ParseDuration(c.Pool.ExpiringSoon); err != nil {
		return fmt.Errorf("pool.expiring_soon: %w", err)
	}
	if c.ExpiringSoonDur < 0 {
		c.ExpiringSoonDur = 0 // 负值视为禁用，避免 upstream 判定窗口反转
	}
	// costTier 探索窗口（issue #136）：空值回落默认 30m（Default 已置；此兜底覆盖
	// 显式 ""）；"0" 是合法值（关停，完全回到现状行为），不回落；负值钳 0 同关停
	// （"−5m" 无合理语义）。
	if c.Pool.CostExploreInterval == "" {
		c.Pool.CostExploreInterval = defaults().Pool.CostExploreInterval
	}
	if c.CostExploreIntervalDur, err = time.ParseDuration(c.Pool.CostExploreInterval); err != nil {
		return fmt.Errorf("pool.cost_explore_interval: %w", err)
	}
	if c.CostExploreIntervalDur < 0 {
		c.CostExploreIntervalDur = 0
	}
	if c.Upstream.TimeoutSeconds <= 0 {
		c.Upstream.TimeoutSeconds = 120
	}
	// header 缺省回落 timeout（保"首字节前换号"既有语义）；idle 缺省走内置大值。
	// 任务书约定：0 一律视为"未设置"走默认，真正的"禁用"留待后续（避免歧义）。
	if c.Upstream.HeaderTimeoutSeconds <= 0 {
		c.Upstream.HeaderTimeoutSeconds = c.Upstream.TimeoutSeconds
	}
	if c.Upstream.IdleTimeoutSeconds <= 0 {
		c.Upstream.IdleTimeoutSeconds = defaults().Upstream.IdleTimeoutSeconds
	}
	if !strings.HasPrefix(c.Listen, ":") && !strings.Contains(c.Listen, ":") {
		c.Listen = ":" + c.Listen
	}
	// fail-fast（设计 supplement §4.1）：admin.enabled=true 且 api_key 为空 = 未鉴权的
	// mutation 端点（disable/revive 是可用性操作，风险高于 /status 读泄漏），拒绝启动。
	// 校验放 applyEnv 之后：env 覆盖（WB2A_ADMIN_ENABLED / WB2A_API_KEY）与 config
	// 两条入口最终状态一致拦截。
	if c.Admin.Enabled && strings.TrimSpace(c.APIKey) == "" {
		return fmt.Errorf("admin.enabled=true 但 api_key 为空：请设置 api_key 或将 admin.enabled 置 false")
	}
	// 排程段归一（空数组回落默认、ActivityReportCount 归一、小时范围校验）
	// 由 internal/config 统一实现，cmd/server 与 cmd/activity 共用同一份语义。
	if err := c.Schedule.Normalize(); err != nil {
		return err
	}
	return c.normalizePrompt()
}

// normalizePrompt 校验 prompt.mode 并按 file 加载提示词文本（custom/append 模式）。
//
// mode 非法（非 custom/append/passthrough）启动报错，避免静默回落到某一分支；
// custom/append 模式下 file 非空但不可读 → 报错（fail fast），file 空 → 用内置默认
// （两模式共用同一加载路径，PromptText 均非空）。
// passthrough 模式不加载文本（透传客户端原始 system，文本在降级时用 prompt.Degraded）。
func (c *Config) normalizePrompt() error {
	switch m := strings.ToLower(strings.TrimSpace(c.Prompt.Mode)); m {
	case "", "passthrough":
		c.Prompt.Mode = "passthrough" // 缺省 passthrough：默认透传客户端原始 system
	case "custom":
		c.Prompt.Mode = "custom"
	case "append":
		c.Prompt.Mode = "append"
	default:
		return fmt.Errorf("prompt.mode: %q 不是合法值（custom / append / passthrough）", c.Prompt.Mode)
	}
	if c.Prompt.Mode == "custom" || c.Prompt.Mode == "append" {
		text, err := prompt.Load(c.Prompt.Mode, c.Prompt.File)
		if err != nil {
			return err
		}
		c.PromptText = text
	}
	return nil
}
