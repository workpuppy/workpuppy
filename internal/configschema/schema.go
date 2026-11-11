// Package configschema 是配置项的**唯一元数据来源**：分组、中文名、类型、范围、
// 枚举、默认值与"是否需重启"。
//
// 它同时服务两处：
//   - 后端：/admin/config 的 PUT/RESET 用它做字段级校验（错误精确到 path）；
//   - 前端：同一份 schema 下发给设置页，渲染控件并做同样的即时校验。
//
// 默认值不在这里硬编码：一律从内嵌的 config.default.json 读取（configdata），
// 保证"默认值只有一处来源"（与 cmd/server.Default() 同源）。
package configschema

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"wp/internal/configdata"
)

// Kind 决定前端控件类型与后端校验方式。
type Kind string

const (
	KindString       Kind = "string"
	KindInt          Kind = "int"
	KindFloat        Kind = "float"
	KindBool         Kind = "bool"
	KindDuration     Kind = "duration"
	KindEnum         Kind = "enum"
	KindStringList   Kind = "stringlist"
	KindDurationList Kind = "durationlist"
	KindTimeList     Kind = "timelist"
	KindStringMap    Kind = "stringmap"
	KindPairs        Kind = "pairs"
)

// Option 枚举项。
type Option struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// Group 设置页里的一个分组（左侧锚点）。
type Group struct {
	Key   string `json:"key"`
	Title string `json:"title"`
	Desc  string `json:"desc"`
}

// Field 一个配置项。
type Field struct {
	Path  string `json:"path"`  // 点分路径，如 pool.max_in_flight
	Group string `json:"group"` // Group.Key
	Label string `json:"label"`
	Hint  string `json:"hint,omitempty"`
	Kind  Kind   `json:"kind"`

	Min *float64 `json:"min,omitempty"`
	Max *float64 `json:"max,omitempty"`

	Options []Option `json:"options,omitempty"`
	Pattern string   `json:"pattern,omitempty"`
	Unit    string   `json:"unit,omitempty"`

	// Secret 敏感值：前端默认用密码框显示，可点"眼睛"看明文。
	Secret bool `json:"secret,omitempty"`
	// Restart 保存后是否需要重启进程才生效（密码与管理面开关为 false）。
	Restart bool `json:"restart"`

	Placeholder string `json:"placeholder,omitempty"`
}

// FieldError 字段级错误：Path 为空表示整份配置的错误。
type FieldError struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

// Spec 下发给前端的整体结构。
type Spec struct {
	Groups []Group `json:"groups"`
	Fields []Field `json:"fields"`
}

func f64(v float64) *float64 { return &v }

var groups = []Group{
	{Key: "basic", Title: "基础与网络", Desc: "监听地址、下游密钥、目录与出口线路。改监听/目录/线路需要重启。"},
	{Key: "admin", Title: "管理后台", Desc: "管理面开关与密码强度。开关与密码即时生效，其余需重启。"},
	{Key: "upstream", Title: "上游请求", Desc: "超时、UA/版本指纹、压缩与设备 token。"},
	{Key: "pool", Title: "账号池与选号", Desc: "并发上限、权重、熔断、降权与大请求轮换。"},
	{Key: "cooldown", Title: "冷却与风控", Desc: "软限流、WAF、404、上游限制与 IP 风控窗口。"},
	{Key: "schedule", Title: "定时任务", Desc: "签到/旅行/活跃/保活/开学/夜猫的执行时刻与节流。"},
	{Key: "state", Title: "状态、缓存与落盘", Desc: "state.json、积分日账的落盘节奏与目录热加载。"},
	{Key: "features", Title: "敏感词净化", Desc: "出站请求体的指纹脱敏与追加规则。"},
	{Key: "prompt", Title: "系统提示词", Desc: "透传 / 替换 / 追加三种模式。"},
	{Key: "upstash", Title: "Redis 缓存", Desc: "会话镜像（可选）。未配置时纯内存运行。"},
	{Key: "models", Title: "模型目录", Desc: "上下文窗口默认值与目录缓存。"},
	{Key: "metrics", Title: "指标与排障", Desc: "指标环形缓冲与请求体落盘阈值。"},
	{Key: "global", Title: "国际版", Desc: "global realm 开关与上游 base 覆盖。"},
	{Key: "session", Title: "会话粘性", Desc: "同会话绑定同账号的 TTL 与 GC。"},
}

var fields = []Field{
	// ── 基础与网络 ────────────────────────────────────────────────
	{Path: "listen", Group: "basic", Label: "监听地址", Kind: KindString,
		Pattern: `^(:[0-9]{1,5}|\[[0-9A-Fa-f:.]+\]:[0-9]{1,5}|[A-Za-z0-9.\-]+:[0-9]{1,5})$`, Placeholder: ":7863",
		Hint: "形如 :7863、127.0.0.1:7863 或 [::]:7863；改完需重启容器才会换端口。", Restart: true},
	{Path: "api_key", Group: "basic", Label: "下游 API Key", Kind: KindString, Secret: true,
		Hint: "/v1 与 /status 用这把 key（与管理密码无关）。空 = 不鉴权。", Restart: true},
	{Path: "auth_dir", Group: "basic", Label: "账号目录", Kind: KindString, Placeholder: "./auths",
		Hint: "存放 auths/*.json；容器内默认 /app/auths。", Restart: true},
	{Path: "state_file", Group: "basic", Label: "状态文件", Kind: KindString, Placeholder: "./data/state.json",
		Hint: "账号池状态快照；同目录还会写 model.json。", Restart: true},
	{Path: "qrcode_ttl", Group: "basic", Label: "扫码二维码有效期", Kind: KindDuration, Unit: "时长",
		Hint: "扫码加号里那张二维码的有效期，如 15m。", Restart: true},
	{Path: "reasoning_as_content", Group: "basic", Label: "思考内容兜底", Kind: KindBool,
		Hint: "整轮没有正文也没有工具调用时，把 reasoning_content 同时作为 content 下发。", Restart: true},
	{Path: "proxies", Group: "basic", Label: "出口线路表", Kind: KindStringMap,
		Hint: "线路名 → 代理地址（http/https/socks5/socks5h）。账号文件里写线路名；查不到会明确报错，不会静默直连。", Restart: true},

	// ── 管理后台 ──────────────────────────────────────────────────
	{Path: "admin.enabled", Group: "admin", Label: "启用管理面", Kind: KindBool, Restart: false,
		Hint: "关闭后 /admin/* 一律 404（设置页也会打不开）：要重新打开只能改配置文件并重启。"},
	{Path: "admin.body_limit_bytes", Group: "admin", Label: "管理面请求体上限", Kind: KindInt,
		Min: f64(1024), Max: f64(1 << 20), Unit: "字节", Hint: "默认 8192。", Restart: true},
	{Path: "admin.min_password_length", Group: "admin", Label: "管理密码最小长度", Kind: KindInt,
		Min: f64(8), Max: f64(128), Restart: false, Hint: "初始化与改密时生效（点保存后立即生效）。"},

	// ── 上游请求 ──────────────────────────────────────────────────
	{Path: "upstream.rpc_timeout_seconds", Group: "upstream", Label: "短 RPC 总超时", Kind: KindInt,
		Min: f64(5), Max: f64(3600), Unit: "秒", Restart: true, Hint: "refresh/checkin/balance/FetchModels；聊天流不受它约束。"},
	{Path: "upstream.header_timeout_seconds", Group: "upstream", Label: "聊天响应头超时", Kind: KindInt,
		Min: f64(0), Max: f64(3600), Unit: "秒", Restart: true, Hint: "<=0 回落短 RPC 总超时。"},
	{Path: "upstream.idle_timeout_seconds", Group: "upstream", Label: "聊天流空闲超时", Kind: KindInt,
		Min: f64(0), Max: f64(3600), Unit: "秒", Restart: true, Hint: "<=0 用默认 300。"},
	{Path: "upstream.user_agent", Group: "upstream", Label: "出站 User-Agent", Kind: KindString, Restart: true,
		Hint: "空 = 用内置三段式（WorkBuddy/<version> …）。"},
	{Path: "upstream.client_version", Group: "upstream", Label: "客户端版本段", Kind: KindString, Restart: true,
		Hint: "UA 里的 WorkBuddy/<ver> 与 X-IDE-Version。"},
	{Path: "upstream.cli_version", Group: "upstream", Label: "CLI 版本段", Kind: KindString, Restart: true},
	{Path: "upstream.device_token", Group: "upstream", Label: "设备风控 Token", Kind: KindString, Secret: true, Restart: true,
		Hint: "X-Device-Token 全局兜底；空 = 不注入。"},
	{Path: "upstream.device_token_file", Group: "upstream", Label: "设备 Token 文件", Kind: KindString, Restart: true},
	{Path: "upstream.client_name", Group: "upstream", Label: "用量归属名", Kind: KindString, Restart: true,
		Hint: "默认 WorkBuddy（伪造官方桌面端指纹）；填 SaaS 还原旧行为。"},
	{Path: "upstream.passthrough_ip", Group: "upstream", Label: "透传客户端 IP", Kind: KindBool, Restart: true},
	{Path: "upstream.gzip_body", Group: "upstream", Label: "压缩聊天请求体", Kind: KindBool, Restart: true},
	{Path: "upstream.retry_after_max", Group: "upstream", Label: "Retry-After 上限", Kind: KindDuration, Restart: true},
	{Path: "upstream.gzip_threshold_bytes", Group: "upstream", Label: "压缩阈值", Kind: KindInt,
		Min: f64(0), Max: f64(16 << 20), Unit: "字节", Restart: true, Hint: "0 = 不压缩。"},
	{Path: "upstream.refresh_io_timeout", Group: "upstream", Label: "Token 刷新超时", Kind: KindDuration, Restart: true},
	{Path: "upstream.device_token_ttl", Group: "upstream", Label: "设备 Token 缓存", Kind: KindDuration, Restart: true},
	{Path: "upstream.device_token_max_len", Group: "upstream", Label: "设备 Token 最大长度", Kind: KindInt,
		Min: f64(16), Max: f64(1 << 16), Unit: "字节", Restart: true},

	// ── 账号池与选号 ──────────────────────────────────────────────
	{Path: "pool.max_in_flight", Group: "pool", Label: "单号在途上限", Kind: KindInt,
		Min: f64(1), Max: f64(64), Restart: true, Hint: "同一账号同时处理的请求数上限。"},
	{Path: "pool.max_in_flight_global", Group: "pool", Label: "global 单号在途上限", Kind: KindInt,
		Min: f64(0), Max: f64(64), Restart: true, Hint: "0 = 回落上面的单号上限。"},
	{Path: "pool.credit_floor", Group: "pool", Label: "保底积分", Kind: KindInt,
		Min: f64(0), Max: f64(1 << 40), Restart: true, Hint: "低于该值的账号只作最后候选；0 = 关闭。"},
	{Path: "pool.breaker_threshold", Group: "pool", Label: "熔断阈值", Kind: KindInt,
		Min: f64(1), Max: f64(100), Unit: "次", Restart: true},
	{Path: "pool.breaker_cooldown", Group: "pool", Label: "熔断基础时长", Kind: KindDuration, Restart: true},
	{Path: "pool.breaker_cooldown_max", Group: "pool", Label: "熔断封顶", Kind: KindDuration, Restart: true},
	{Path: "pool.degrade_threshold", Group: "pool", Label: "连败降权阈值", Kind: KindInt,
		Min: f64(1), Max: f64(100), Unit: "次", Restart: true},
	{Path: "pool.degrade_cooldown", Group: "pool", Label: "降权时长", Kind: KindDuration, Restart: true},
	{Path: "pool.degrade_cooldown_max", Group: "pool", Label: "降权封顶", Kind: KindDuration, Restart: true},
	{Path: "pool.idle_weight_per_hour", Group: "pool", Label: "闲置补偿/小时", Kind: KindFloat,
		Min: f64(0), Max: f64(100), Restart: true},
	{Path: "pool.idle_weight_max", Group: "pool", Label: "闲置补偿封顶", Kind: KindFloat,
		Min: f64(0), Max: f64(1000), Restart: true},
	{Path: "pool.expiring_soon_window", Group: "pool", Label: "快过期窗口", Kind: KindDuration, Restart: true,
		Hint: "到期时间落在窗口内的积分优先消耗；0 关闭分桶。"},
	{Path: "pool.expiring_weight", Group: "pool", Label: "快过期积分权重倍数", Kind: KindFloat,
		Min: f64(0), Max: f64(1000), Restart: true},
	{Path: "pool.cost_explore_interval", Group: "pool", Label: "成本层探索窗口", Kind: KindDuration, Restart: true,
		Hint: "0 = 关闭探索。"},
	{Path: "pool.min_pick_gap", Group: "pool", Label: "选号防惊群间隔", Kind: KindDuration, Restart: true},
	{Path: "pool.session_dead_threshold", Group: "pool", Label: "session dead 禁用阈值", Kind: KindInt,
		Min: f64(1), Max: f64(100), Unit: "次", Restart: true},
	{Path: "pool.rotation_backoff_base", Group: "pool", Label: "换号退避基数", Kind: KindDuration, Restart: true},
	{Path: "pool.large_request_bytes", Group: "pool", Label: "大请求判定阈值", Kind: KindInt,
		Min: f64(0), Max: f64(1 << 26), Unit: "字节", Restart: true, Hint: "0 = 关闭大请求策略。"},
	{Path: "pool.large_request_cooldown", Group: "pool", Label: "大请求轮换窗口", Kind: KindDuration, Restart: true,
		Hint: ">0 才启用逐请求换号。"},
	{Path: "pool.large_request_budget", Group: "pool", Label: "大请求预算", Kind: KindInt,
		Min: f64(0), Max: f64(1000), Unit: "条", Restart: true, Hint: ">0 时按滑动窗口限每号条数。"},
	{Path: "pool.large_request_budget_window", Group: "pool", Label: "大请求预算窗口", Kind: KindDuration, Restart: true},

	// ── 冷却与风控 ────────────────────────────────────────────────
	{Path: "cooldown.soft_rate", Group: "cooldown", Label: "软限流冷却基数", Kind: KindDuration, Restart: true},
	{Path: "cooldown.soft_rate_max", Group: "cooldown", Label: "软冷却封顶", Kind: KindDuration, Restart: true},
	{Path: "cooldown.credit_exhausted", Group: "cooldown", Label: "积分耗尽冷却", Kind: KindDuration, Restart: true,
		Hint: "空 = 硬冷却到次日 04:00；填了就只退出这一小段。"},
	{Path: "cooldown.not_found", Group: "cooldown", Label: "404 冷却", Kind: KindDuration, Restart: true},
	{Path: "cooldown.waf_base", Group: "cooldown", Label: "WAF 403 基础冷却", Kind: KindDuration, Restart: true},
	{Path: "cooldown.upstream_restriction", Group: "cooldown", Label: "上游限制冷却", Kind: KindDuration, Restart: true},
	{Path: "risk.waf_window", Group: "cooldown", Label: "IP 风控窗口", Kind: KindDuration, Restart: true},
	{Path: "risk.waf_threshold", Group: "cooldown", Label: "IP 风控阈值", Kind: KindInt,
		Min: f64(1), Max: f64(1000), Unit: "次", Restart: true},

	// ── 定时任务 ──────────────────────────────────────────────────
	{Path: "schedule.checkin_time", Group: "schedule", Label: "签到时刻", Kind: KindTimeList, Restart: true},
	{Path: "schedule.travel_time", Group: "schedule", Label: "猫猫旅行时刻", Kind: KindTimeList, Restart: true},
	{Path: "schedule.activity_time", Group: "schedule", Label: "活跃上报时刻", Kind: KindTimeList, Restart: true},
	{Path: "schedule.keepalive_time", Group: "schedule", Label: "Token 保活时刻", Kind: KindTimeList, Restart: true},
	{Path: "schedule.school_time", Group: "schedule", Label: "开学季时刻", Kind: KindTimeList, Restart: true},
	{Path: "schedule.cat_time", Group: "schedule", Label: "夜猫子时刻", Kind: KindTimeList, Restart: true},
	{Path: "schedule.checkin_enabled", Group: "schedule", Label: "启用签到", Kind: KindBool, Restart: true},
	{Path: "schedule.travel_enabled", Group: "schedule", Label: "启用猫猫旅行", Kind: KindBool, Restart: true},
	{Path: "schedule.activity_enabled", Group: "schedule", Label: "启用活跃上报", Kind: KindBool, Restart: true},
	{Path: "schedule.keepalive_enabled", Group: "schedule", Label: "启用 Token 保活", Kind: KindBool, Restart: true},
	{Path: "schedule.school_enabled", Group: "schedule", Label: "启用开学季", Kind: KindBool, Restart: true},
	{Path: "schedule.cat_enabled", Group: "schedule", Label: "启用夜猫子", Kind: KindBool, Restart: true},
	{Path: "schedule.activity_report_count", Group: "schedule", Label: "每号上报条数", Kind: KindInt,
		Min: f64(1), Max: f64(50), Unit: "条", Restart: true},
	{Path: "schedule.spread_window", Group: "schedule", Label: "账号随机分布窗口", Kind: KindDuration, Restart: true,
		Hint: "0 = 到点后按固定间隔顺序执行。"},
	{Path: "schedule.intra_request_jitter_min", Group: "schedule", Label: "同号请求间隔下限", Kind: KindDuration, Restart: true},
	{Path: "schedule.intra_request_jitter_max", Group: "schedule", Label: "同号请求间隔上限", Kind: KindDuration, Restart: true},
	{Path: "schedule.random_delay_minutes", Group: "schedule", Label: "随机延迟上限", Kind: KindInt,
		Min: f64(0), Max: f64(1440), Unit: "分钟", Restart: true},
	{Path: "schedule.checkin_account_delay", Group: "schedule", Label: "签到相邻账号间隔", Kind: KindDuration, Restart: true},
	{Path: "schedule.travel_account_delay", Group: "schedule", Label: "旅行相邻账号间隔", Kind: KindDuration, Restart: true},
	{Path: "schedule.activity_account_delay", Group: "schedule", Label: "上报相邻账号间隔", Kind: KindDuration, Restart: true},
	{Path: "schedule.checkin_retry_delays", Group: "schedule", Label: "签到重试间隔", Kind: KindDurationList, Restart: true},
	{Path: "schedule.checkin_refresh_skew", Group: "schedule", Label: "签到前刷新提前量", Kind: KindDuration, Restart: true},
	{Path: "schedule.wakeup_grace", Group: "schedule", Label: "迟到唤醒宽限", Kind: KindDuration, Restart: true},
	{Path: "schedule.catchup_window", Group: "schedule", Label: "补跑窗口", Kind: KindDuration, Restart: true},
	{Path: "schedule.account_delay_min_seconds", Group: "schedule", Label: "账号延迟下限", Kind: KindInt,
		Min: f64(0), Max: f64(3600), Unit: "秒", Restart: true},
	{Path: "schedule.account_delay_max_seconds", Group: "schedule", Label: "账号延迟上限", Kind: KindInt,
		Min: f64(0), Max: f64(3600), Unit: "秒", Restart: true},

	// ── 状态、缓存与落盘 ──────────────────────────────────────────
	{Path: "state.flush_interval", Group: "state", Label: "关键状态落盘间隔", Kind: KindDuration, Restart: true},
	{Path: "state.soft_flush_interval", Group: "state", Label: "软计数落盘间隔", Kind: KindDuration, Restart: true},
	{Path: "state.credit_flush_interval", Group: "state", Label: "积分日账落盘间隔", Kind: KindDuration, Restart: true},
	{Path: "auths.watch_interval", Group: "state", Label: "账号目录热加载间隔", Kind: KindDuration, Restart: true},

	// ── 敏感词净化 ────────────────────────────────────────────────
	{Path: "features.sanitize_blacklist_fingerprints", Group: "features", Label: "指纹脱敏", Kind: KindBool, Restart: true},
	{Path: "features.sanitize_extra_features", Group: "features", Label: "追加特征串", Kind: KindStringList, Restart: true,
		Hint: "命中任一即进入净化；空 = 只用内置静态表。"},
	{Path: "features.sanitize_extra_rewrites", Group: "features", Label: "追加逐字替换", Kind: KindPairs, Restart: true,
		Hint: "每行一组「原串 → 替换串」，精确替换、大小写敏感。"},

	// ── 系统提示词 ────────────────────────────────────────────────
	{Path: "prompt.mode", Group: "prompt", Label: "提示词模式", Kind: KindEnum, Restart: true,
		Options: []Option{{Value: "passthrough", Label: "透传客户端 system"}, {Value: "custom", Label: "替换为网关提示词"}, {Value: "append", Label: "追加网关提示词"}}},
	{Path: "prompt.file", Group: "prompt", Label: "提示词文件", Kind: KindString, Restart: true,
		Hint: "空 = 内置默认；custom/append 模式下文件读不到会启动失败。"},

	// ── Redis 缓存 ────────────────────────────────────────────────
	{Path: "upstash.url", Group: "upstash", Label: "Redis 地址", Kind: KindString, Restart: true,
		Hint: "留空 = 纯内存模式；支持 rediss:// 完整连接串。"},
	{Path: "upstash.token", Group: "upstash", Label: "Redis Token", Kind: KindString, Secret: true, Restart: true},
	{Path: "upstash.key_ttl", Group: "upstash", Label: "会话镜像 TTL", Kind: KindDuration, Restart: true},
	{Path: "upstash.write_concurrency", Group: "upstash", Label: "并发写入上限", Kind: KindInt,
		Min: f64(1), Max: f64(64), Restart: true},

	// ── 模型目录 ──────────────────────────────────────────────────
	{Path: "models.default_context_window", Group: "models", Label: "默认上下文窗口", Kind: KindInt,
		Min: f64(1000), Max: f64(100000000), Unit: "token", Restart: true},
	{Path: "models.catalog_timeout", Group: "models", Label: "目录拉取超时", Kind: KindDuration, Restart: true},
	{Path: "models.catalog_refresh_cooldown", Group: "models", Label: "目录刷新冷却", Kind: KindDuration, Restart: true},
	{Path: "models.catalog_negative_ttl", Group: "models", Label: "目录负缓存 TTL", Kind: KindDuration, Restart: true},

	// ── 指标与排障 ────────────────────────────────────────────────
	{Path: "metrics.capacity", Group: "metrics", Label: "指标环形缓冲容量", Kind: KindInt,
		Min: f64(16), Max: f64(1 << 16), Restart: true},
	{Path: "debug.dump_min_bytes", Group: "metrics", Label: "请求体落盘阈值", Kind: KindInt,
		Min: f64(0), Max: f64(1 << 30), Unit: "字节", Restart: true, Hint: "0 = 关闭。"},

	// ── 国际版 ────────────────────────────────────────────────────
	{Path: "global.enabled", Group: "global", Label: "启用 global realm", Kind: KindBool, Restart: true},
	{Path: "global.chat_base", Group: "global", Label: "global 聊天 base", Kind: KindString, Restart: true,
		Hint: "空 = 内置默认 https://www.workbuddy.ai。"},
	{Path: "global.billing_base", Group: "global", Label: "global 计费 base", Kind: KindString, Restart: true},

	// ── 会话粘性 ──────────────────────────────────────────────────
	{Path: "session_sticky.enabled", Group: "session", Label: "启用会话粘性", Kind: KindBool, Restart: true},
	{Path: "session_sticky.ttl", Group: "session", Label: "绑定 TTL", Kind: KindDuration, Restart: true},
	{Path: "session_sticky.gc_interval", Group: "session", Label: "GC 周期", Kind: KindDuration, Restart: true},
}

// Schema 返回完整 schema（供 /admin/config 下发）。
func Schema() Spec { return Spec{Groups: groups, Fields: fields} }

// Fields 返回字段列表（只读）。
func Fields() []Field { return fields }

// Lookup 按 path 查字段。
func Lookup(path string) (Field, bool) {
	for _, f := range fields {
		if f.Path == path {
			return f, true
		}
	}
	return Field{}, false
}

var (
	defaultsOnce sync.Once
	defaultsMap  map[string]any
)

// Defaults 返回内嵌 config.default.json 解析出的默认值（嵌套 map）。
func Defaults() map[string]any {
	defaultsOnce.Do(func() {
		var m map[string]any
		if err := json.Unmarshal(configdata.Bytes(), &m); err != nil {
			panic("configschema: 内嵌 config.default.json 无法解析: " + err.Error())
		}
		defaultsMap = m
	})
	return defaultsMap
}

// DefaultAt 取某个 path 的默认值。
func DefaultAt(path string) (any, bool) { return GetPath(Defaults(), path) }

// GetPath 按点分路径取值。
func GetPath(doc map[string]any, path string) (any, bool) {
	cur := any(doc)
	for _, seg := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[seg]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// SetPath 按点分路径写值（自动创建中间对象）。
func SetPath(doc map[string]any, path string, val any) {
	segs := strings.Split(path, ".")
	cur := doc
	for _, seg := range segs[:len(segs)-1] {
		next, ok := cur[seg].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[seg] = next
		}
		cur = next
	}
	cur[segs[len(segs)-1]] = val
}

// DeletePath 删除某路径（不存在则无操作）；空对象会被顺手清掉，保持文件干净。
func DeletePath(doc map[string]any, path string) {
	segs := strings.Split(path, ".")
	stack := []map[string]any{doc}
	cur := doc
	for _, seg := range segs[:len(segs)-1] {
		next, ok := cur[seg].(map[string]any)
		if !ok {
			return
		}
		cur = next
		stack = append(stack, cur)
	}
	delete(cur, segs[len(segs)-1])
	for i := len(stack) - 1; i > 0; i-- {
		if len(stack[i]) == 0 {
			delete(stack[i-1], segs[i-1])
		}
	}
}

// ValidateDoc 对"合并后的文档"做字段级校验，返回所有能发现的问题。
//
// 只校验 schema 里声明过的字段；未知键不在这里报（启动时另有 WARN）。
func ValidateDoc(doc map[string]any) []FieldError {
	var errs []FieldError
	for _, f := range fields {
		v, ok := GetPath(doc, f.Path)
		if !ok || v == nil {
			continue
		}
		if msg := validateValue(f, v); msg != "" {
			errs = append(errs, FieldError{Path: f.Path, Message: msg})
		}
	}
	return errs
}

func validateValue(f Field, v any) string {
	switch f.Kind {
	case KindBool:
		if _, ok := v.(bool); !ok {
			return "应为布尔值（true / false）"
		}
	case KindString, KindEnum:
		s, ok := v.(string)
		if !ok {
			return "应为字符串"
		}
		if f.Kind == KindEnum && !hasOption(f.Options, s) {
			return "取值不在允许范围内"
		}
		if f.Pattern != "" && !matchPattern(f.Pattern, s) {
			return "格式不正确"
		}
	case KindInt:
		n, ok := jsonNumber(v)
		if !ok || math.Trunc(n) != n {
			return "应为整数"
		}
		if msg := rangeCheck(f, n); msg != "" {
			return msg
		}
	case KindFloat:
		n, ok := jsonNumber(v)
		if !ok {
			return "应为数字"
		}
		if msg := rangeCheck(f, n); msg != "" {
			return msg
		}
	case KindDuration:
		s, ok := v.(string)
		if !ok {
			return "应为时长字符串（如 30s / 5m / 2h）"
		}
		if strings.TrimSpace(s) == "" {
			return "" // 空 = 未设置（是否允许由业务语义决定）
		}
		d, err := time.ParseDuration(strings.TrimSpace(s))
		if err != nil {
			return "时长格式不正确（示例：500ms / 30s / 5m / 2h）"
		}
		if d < 0 {
			return "不能为负（关闭请写 0）"
		}
	case KindStringList:
		arr, ok := v.([]any)
		if !ok {
			return "应为字符串数组"
		}
		for _, item := range arr {
			if _, ok := item.(string); !ok {
				return "数组元素都应是字符串"
			}
		}
	case KindDurationList:
		arr, ok := v.([]any)
		if !ok {
			return "应为时长数组"
		}
		for _, item := range arr {
			s, ok := item.(string)
			if !ok {
				return "数组元素都应是时长字符串"
			}
			if _, err := time.ParseDuration(strings.TrimSpace(s)); err != nil {
				return fmt.Sprintf("时长格式不正确：%q", s)
			}
		}
	case KindTimeList:
		arr, ok := v.([]any)
		if !ok {
			return "应为时刻数组"
		}
		for _, item := range arr {
			s, ok := item.(string)
			if !ok || !validClock(s) {
				return "时刻格式应为 HH:MM（00:00–23:59）"
			}
		}
	case KindStringMap:
		m, ok := v.(map[string]any)
		if !ok {
			return "应为「键 → 值」对象"
		}
		for k, item := range m {
			if strings.TrimSpace(k) == "" {
				return "键不能为空"
			}
			s, ok := item.(string)
			if !ok || strings.TrimSpace(s) == "" {
				return fmt.Sprintf("%s 的值必须是非空字符串", k)
			}
		}
	case KindPairs:
		arr, ok := v.([]any)
		if !ok {
			return "应为「[原串, 替换串]」数组"
		}
		for _, item := range arr {
			pair, ok := item.([]any)
			if !ok || len(pair) != 2 {
				return "每项必须是两个字符串组成的数组"
			}
			for _, p := range pair {
				if _, ok := p.(string); !ok {
					return "替换对里必须都是字符串"
				}
			}
		}
	}
	return ""
}

func hasOption(opts []Option, v string) bool {
	for _, o := range opts {
		if o.Value == v {
			return true
		}
	}
	return false
}

func matchPattern(pattern, s string) bool {
	if pattern == "" {
		return true
	}
	ok, err := regexp.MatchString(pattern, s)
	if err != nil {
		return true // schema 自身写错时不阻塞保存（由测试兜底）
	}
	return ok
}

func rangeCheck(f Field, n float64) string {
	if f.Min != nil && n < *f.Min {
		return fmt.Sprintf("不能小于 %s", trimNum(*f.Min))
	}
	if f.Max != nil && n > *f.Max {
		return fmt.Sprintf("不能大于 %s", trimNum(*f.Max))
	}
	return ""
}

func trimNum(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

func jsonNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

func validClock(s string) bool {
	s = strings.TrimSpace(s)
	parts := strings.Split(s, ":")
	if len(parts) != 2 {
		return false
	}
	h, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	return err1 == nil && err2 == nil && h >= 0 && h <= 23 && m >= 0 && m <= 59
}
