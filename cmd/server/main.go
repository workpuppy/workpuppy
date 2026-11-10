// main.go wp 入口：加载配置、构建 pool、起调度器与 HTTP 服务。
package main

import (
	"context"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"wp/internal/adminauth"
	"wp/internal/auth"
	"wp/internal/loginsvc"
	"wp/internal/pool"
	"wp/internal/redisstore"
	"wp/internal/scheduler"
	"wp/internal/server"
	"wp/internal/session"
	"wp/internal/upstream"
	"wp/internal/webui"
)

// modelJSONPath 由 state.json 路径推导 model.json 路径（同目录同名换缀）：
// 两者同为数据目录持久化物（Docker ./data volume），配套而非各自配置。
// state 路径为空（纯内存测试形态）→ 空 = 禁用 model.json 落盘（内存 + 种子仍可用）。
func modelJSONPath(stateFile string) string {
	if stateFile == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(stateFile), "model.json")
}

func main() {
	cfgPath := flag.String("config", "config.json", "path to config json")
	flag.Parse()

	cfg, err := Load(*cfgPath)
	if err != nil {
		// 配置文件不存在时给一次机会用纯默认 + env
		if os.IsNotExist(err) {
			log.Printf("config %s not found, using defaults+env", *cfgPath)
			cfg, err = Load("")
		}
		if err != nil {
			log.Fatalf("load config: %v", err)
		}
	}

	auths, err := auth.LoadDir(cfg.AuthDir)
	if err != nil {
		log.Fatalf("load auths: %v", err)
	}
	log.Printf("loaded %d account(s) from %s", len(auths), cfg.AuthDir)

	// global realm 路由开关（config global.enabled，缺省 true）：注入 auth 包全局闸。
	// Realm()/IsGlobal() 先过此闸——显式 false 时恒 cn（逃生门：纯 CN 锁定的第一道闸）。
	auth.SetGlobalEnabled(cfg.Global.Enabled)

	// model.json 本地缓存接线（context_length 四级查找链第 3 级）：数据目录与
	// state.json 同风格（Docker volume 持久化路径 ./data）。首次缺失/损坏自动回落
	// 仓库种子 embed；models.dev 按需拉取成功后原子写回。
	upstream.SetModelCatalogPath(modelJSONPath(cfg.StateFile))

	// redisstore：未配置/连接失败 → Noop（纯内存模式，一切功能照常）。
	store := redisstore.New(cfg.Upstash.URL, cfg.Upstash.Token)

	p := pool.New(cfg.StateFile)
	defer p.Close() // 进程退出前停后台落盘 goroutine + 最后补一次落盘（FIX-4:goroutine 泄漏）
	p.SetStore(store)
	p.RestoreFromSnapshot() // 择新恢复：Redis 快照比本地新才采用，否则本地优先
	p.SyncToDir(auths)      // 与 auths 目录对齐：新账号加入、已删除文件账号剔除（状态保留）

	// auths 目录热加载：新增凭证文件自动进池，免去「加完账号手动重启网关」。
	// 启动时的 SyncToDir 已建立基线，监听只在后续目录内容变化时触发（见 pool/watch.go）。
	stopWatch := p.StartAuthDirWatch(cfg.AuthDir)
	defer stopWatch()

	// 熔断器 + 在途上限 + 三因子加权调优（从 config 注入，非正值回退默认）。
	p.SetBreaker(cfg.Pool.BreakerThreshold, cfg.BreakerCooldownDur, cfg.BreakerCooldownMaxD)
	// 连败降权（issue #114）：ErrClient/传输层连败 N 次临时出池。
	p.SetDegrade(cfg.Pool.DegradeThreshold, cfg.DegradeCooldownDur, cfg.DegradeCooldownMaxD)
	p.SetMaxInFlight(cfg.Pool.MaxInFlight)
	p.SetMaxInFlightGlobal(cfg.Pool.MaxInFlightGlobal) // global 域在途分档（WAF 403 修复 P1-1，默认 2）
	p.SetSoftRateMax(cfg.SoftRateMaxDur)               // 软冷却指数退避封顶（soft_rate_max，默认 2h）
	// 账号级出口线路（issue #210）：面板绑在线路名上，这里把名字解析成地址。
	upstream.ConfigureProxies(cfg.Proxies)
	// 思考通道兜底（issue #135）：默认开启；显式关掉时打一行日志，免得"为什么
	// 只看 content 的客户端又空了"变成悬案。
	upstream.ConfigureReasoningFallback(cfg.ReasoningAsContent == nil || *cfg.ReasoningAsContent)
	if !upstream.ReasoningFallbackEnabled() {
		log.Printf("思考通道兜底已关闭：只含 reasoning_content 的回复不会再补成 content")
	}
	if names := upstream.RouteNames(); len(names) > 0 {
		log.Printf("出口线路已加载：%d 条（%s）—— 账号绑定的线路名按它解析",
			len(names), strings.Join(names, ", "))
	} else {
		log.Printf("未配置出口线路（config.json 的 proxies 段）：账号绑定的线路名会明确报错，不会静默直连")
	}
	p.SetCreditExhausted(cfg.CreditExhaustedDur) // 积分耗尽后的冷却时长（credit_exhausted，0 = 旧行为）               // 软冷却指数退避封顶（soft_rate_max，默认 2h）
	p.SetWeights(cfg.Pool.IdleWeightPerHour, cfg.Pool.IdleWeightMax)
	p.SetCostExploreInterval(cfg.CostExploreIntervalDur) // costTier 探索窗口（issue #136，默认 30m；0 关停）

	// 会话粘性路由（可配关闭）。
	var sessRouter *session.Router
	redisMode := "noop"
	if _, ok := store.(redisstore.Noop); !ok {
		redisMode = "upstash"
	}
	if cfg.SessionSticky.Enabled {
		sessRouter = session.New(session.Config{
			TTL:        cfg.SessionTTL,
			GCInterval: cfg.SessionGCInterval,
			Store:      store,
			Available:  p.AvailableUIDs,
			// 按模型的可用性口径：绑定号在当前模型被 6004 限额时重分配，
			// 而不是被钉在这个号上反复失败。
			// realm 感知闭包：带前缀模型名按 realm 过滤可用账号（跨 realm 不泄漏，
			// 见 wiring.go）；裸名走 cn（现状零回归）。
			AvailableForModel: realmAwareAvailableForModel(p),
			// 首绑按三因子权重分配（issue #265）：粘性命中即定，若首绑仍按哈希取模，
			// 「快过期积分优先消耗」这条池权重在默认配置下就永远轮不到——带会话标识的
			// 请求首轮绑定后滚动续期，压根走不到 Pool.pick。
			PickForModel: realmAwareWeightedPick(p),
		})
		sessRouter.LoadFromStore() // 启动时从 Redis 恢复粘性（读操作仅此处）
		sessRouter.StartGC()
		defer sessRouter.StopGC()
	}
	sessCount := func() int {
		if sessRouter != nil {
			return sessRouter.Count()
		}
		return 0
	}

	up := upstream.New()
	up.RefreshPaused = p.RiskPaused
	up.OnRefreshRateLimit = func(uid string, retryAfter time.Duration) { p.PauseRefreshRateLimited(uid, retryAfter) }
	up.OnRefreshSuccess = p.NoteRefreshSuccess
	// 短 RPC 总时长上限（refresh/checkin/balance/FetchModels），语义不变。
	up.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	// 聊天 SSE 首字节前（响应头）上限：cfg 已 normalize（缺省回落 timeout_seconds）。
	up.HeaderTimeout = time.Duration(cfg.Upstream.HeaderTimeoutSeconds) * time.Second
	if tr, ok := up.ChatHTTP.Transport.(*http.Transport); ok {
		tr.ResponseHeaderTimeout = up.HeaderTimeout
	}
	// 聊天 SSE 流中空闲上限（S3 空闲监控读取）。
	up.IdleTimeout = time.Duration(cfg.Upstream.IdleTimeoutSeconds) * time.Second
	up.SanitizeFingerprints = cfg.Features.SanitizeBlacklistFingerprints
	// 运行期追加的黑名单词表（features.sanitize_extra_features / _rewrites）：
	// 上游逐词扩充黑名单时改 config.json 重启即可，不必重编译镜像。
	// 空表 = 只用内置静态表（行为与旧版一致）。
	upstream.ConfigureSanitize(cfg.Features.SanitizeExtraFeatures, cfg.Features.SanitizeExtraRewrites)
	if n := len(cfg.Features.SanitizeExtraFeatures); n > 0 {
		log.Printf("净化额外特征词表已加载：%d 条（config.json features.sanitize_extra_features）", n)
	}
	// 出站 UA（A 段）：非空才做显式覆盖，空 = 默认 WorkBuddy 三段式
	// `WorkBuddy/<client_version> WorkBuddy/<client_version> CLI/<cli_version>`。
	up.UserAgent = cfg.Upstream.UserAgent
	// 版本段（upstream.client_version / cli_version）：空 = 各走内置默认。
	up.ClientVersion = cfg.Upstream.ClientVersion
	up.CliVersion = cfg.Upstream.CliVersion
	// 设备风控头（X-Device-Token）全局兜底 + 文件读取路径；空 = 不注入。
	up.DeviceToken = cfg.Upstream.DeviceToken
	up.DeviceTokenFile = cfg.Upstream.DeviceTokenFile
	// 用量归属头（X-Product/X-IDE-*）+ 客户端 IP 透传开关（见 ChatHeaders / handler）。
	up.ClientName = cfg.Upstream.ClientName
	up.PassthroughIP = cfg.Upstream.PassthroughIP
	// 大请求体 gzip（issue #297）：>64KB 才压，短请求零开销。
	up.GzipBody = cfg.Upstream.GzipBody
	// global realm 双域路由（config global 段）：base 空回落内置默认 https://www.workbuddy.ai；
	// GlobalEnabled 与 auth 包开关一致（双保险第二道闸在 upstream.globalOn）。
	up.ChatBaseGlobal = cfg.Global.ChatBase
	up.BillingBaseGlobal = cfg.Global.BillingBase
	up.GlobalEnabled = cfg.Global.Enabled

	// The private deployment identity makes the daily random offsets stable
	// across restarts, and independent of other deployments. Never log the seed.
	scheduleSeed := cfg.APIKey
	if scheduleSeed == "" {
		scheduleSeed = cfg.AuthDir
	}
	// 遍历节奏：默认值来自 config.default.json，用户 config.json 有则覆盖。
	scheduler.Configure(scheduler.Options{
		CheckinAccountDelay:  cfg.Schedule.CheckinAccountDelayDur,
		TravelAccountDelay:   cfg.Schedule.TravelAccountDelayDur,
		ActivityAccountDelay: cfg.Schedule.ActivityAccountDelayDur,
		CheckinRefreshSkew:   cfg.Schedule.CheckinRefreshSkewDur,
		CheckinRetryDelays:   cfg.Schedule.CheckinRetryDelaysDur,
		WakeupGrace:          cfg.Schedule.WakeupGraceDur,
		CatchupWindow:        cfg.Schedule.CatchupWindowDur,
	})
	sch := scheduler.New(scheduler.Config{
		AccountDelayMinSeconds: cfg.Schedule.AccountDelayMinSeconds,
		AccountDelayMaxSeconds: cfg.Schedule.AccountDelayMaxSeconds,
		RandomDelayMinutes:     cfg.Schedule.RandomDelayMinutes,
		RandomDelaySeed:        scheduleSeed,
		Pool:                   p,
		Upstream:               up,
		CheckinAt:              cfg.Schedule.CheckinAt,
		TravelAt:               cfg.Schedule.TravelAt,
		ActivityAt:             cfg.Schedule.ActivityAt,
		KeepaliveAt:            cfg.Schedule.KeepaliveAt,
		SchoolAt:               cfg.Schedule.SchoolAt,
		CatAt:                  cfg.Schedule.CatAt,
		ActivityReportCount:    cfg.Schedule.ActivityReportCount,
		ExpiringSoonWindow:     cfg.ExpiringSoonDur, // 快过期积分优先消耗（issue:积分过期）
		SpreadWindow:           cfg.SpreadWindowDur, // 定时任务账号随机分布窗口（schedule.spread_window）
		IntraJitterMin:         cfg.IntraJitterMinD, // 同账号连续请求的随机间隔下限
		IntraJitterMax:         cfg.IntraJitterMaxD, // 上限
		CheckinDisabled:        !cfg.Schedule.CheckinEnabled,
		TravelDisabled:         !cfg.Schedule.TravelEnabled,
		ActivityDisabled:       !cfg.Schedule.ActivityEnabled,
		KeepaliveDisabled:      !cfg.Schedule.KeepaliveEnabled,
		SchoolDisabled:         !cfg.Schedule.SchoolEnabled,
		CatDisabled:            !cfg.Schedule.CatEnabled,
	})
	switch {
	case !cfg.Schedule.CheckinEnabled:
		log.Printf("签到已禁用（schedule.checkin_enabled=false）")
	default:
		log.Printf("签到已启用：%v（签到 + 余额查询解冻）", scheduleTimes(cfg.Schedule.CheckinTime))
	}
	switch {
	case !cfg.Schedule.TravelEnabled:
		log.Printf("猫猫旅行已禁用（schedule.travel_enabled=false）")
	default:
		log.Printf("猫猫旅行已启用：%v（独立排程：领养 / 派出 / 领奖）", scheduleTimes(cfg.Schedule.TravelTime))
	}
	switch {
	case !cfg.Schedule.ActivityEnabled:
		log.Printf("活跃上报已禁用（schedule.activity_enabled=false）")
	default:
		log.Printf("活跃上报已启用：%v（每号 %d 条，点亮连登 + 补满领猫对话门槛）", scheduleTimes(cfg.Schedule.ActivityTime), cfg.Schedule.ActivityReportCount)
	}
	if !cfg.Schedule.KeepaliveEnabled {
		log.Printf("token 保活已禁用（schedule.keepalive_enabled=false）")
	} else {
		log.Printf("token 保活已启用：%v", scheduleTimes(cfg.Schedule.KeepaliveTime))
	}
	if !cfg.Schedule.SchoolEnabled {
		log.Printf("开学季任务已禁用（schedule.school_enabled=false）")
	} else {
		log.Printf("开学季任务已启用：%v（school_bin ALL --run --yes）", scheduleTimes(cfg.Schedule.SchoolTime))
	}
	if !cfg.Schedule.CatEnabled {
		log.Printf("夜猫子任务已禁用（schedule.cat_enabled=false）")
	} else {
		log.Printf("夜猫子任务已启用：%v（task_runner ALL --yes --only black_cat）", scheduleTimes(cfg.Schedule.CatTime))
	}
	// 账号随机分布窗口：让"到点后整池账号同一秒打上游"不再发生（见 internal/scheduler/spread.go）。
	if cfg.SpreadWindowDur > 0 {
		log.Printf("定时任务随机分布：每个账号执行时刻 = 整点 + 0~%v 内的随机延迟（schedule.spread_window）",
			cfg.SpreadWindowDur)
	} else {
		log.Printf("定时任务随机分布已关闭（schedule.spread_window=0）：到点后按固定间隔顺序执行")
	}
	if cfg.IntraJitterMaxD > 0 {
		log.Printf("同账号连续请求间隔：每次 `%v ~ %v` 随机（schedule.intra_request_jitter_min/max）",
			cfg.IntraJitterMinD, cfg.IntraJitterMaxD)
	} else {
		log.Printf("同账号连续请求间隔：未加随机间隔（schedule.intra_request_jitter_min/max 均为 0）")
	}

	// 管理面凭据：每次启动随机生成（只存内存），启动完成后打印到终端。
	// WP_ADMIN_PASSWORD 可固定密码（开发/自动化用，长度不足会启动失败）。
	adminStore, adminErr := adminauth.New(os.Getenv("WP_ADMIN_PASSWORD"))
	if adminErr != nil {
		log.Fatalf("管理密码配置无效: %v", adminErr)
	}

	h := server.NewHandler(server.Config{
		Pool:                     p,
		Upstream:                 up,
		APIKey:                   cfg.APIKey,
		AdminAuth:                adminStore,
		Session:                  sessRouter,
		StickyCount:              sessCount,
		RedisMode:                redisMode,
		SoftCooldown:             cfg.SoftRateDur,
		LargeRequestBytes:        cfg.Pool.LargeRequestBytes,
		LargeRequestCooldown:     cfg.LargeRequestCooldownDur,
		LargeRequestBudget:       cfg.Pool.LargeRequestBudget,
		LargeRequestBudgetWindow: cfg.LargeRequestBudgetWindowDur,
		CreditFloor:              cfg.Pool.CreditFloor,
		PromptMode:               cfg.Prompt.Mode,
		PromptText:               cfg.PromptText,
		// global realm 开关（handler 侧第三道闸：modelList 据此决定是否列 global 名单）。
		GlobalEnabled: cfg.Global.Enabled,
		// 运维管理端点开关（config admin.enabled，默认 false）。
		AdminEnabled: cfg.Admin.Enabled,
		// 管理面（REST）接线：调度器（立即签到复用其单飞锁与账号间隔）、
		// 账号目录（扫码加号落盘位置）、登录服务（二维码 state 的内存管理器）。
		Scheduler: sch,
		AuthDir:   cfg.AuthDir,
		Login:     newLoginManager(cfg.QRCodeTTLDur),
		// 管理后台（Bootstrap 静态页，embed 进二进制）：/ 是入口，
		// /assets/... 是内嵌资源，/v1 与 /admin 走各自的 handler。
		UI: webui.Handler(),
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go sch.Run(ctx)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           h,
		ReadHeaderTimeout: 30 * time.Second,
		// ReadTimeout 覆盖整个请求读取（含 body）：防慢速 body 拖死连接。
		// max_body_mb 已移除（请求体无上限，交由上游自然响应），超大 body 成为
		// 唯一的自然约束：60s 内传不完会得到连接错误（read timeout）而非 413。
		ReadTimeout: 60 * time.Second,
		// IdleTimeout keep-alive 空闲连接回收：配合 ctx 传播（FIX-2）防连接泄漏堆积。
		// 注意：SSE 流式响应期间连接非空闲，不受此项掐断；不设全局 WriteTimeout
		// （长流式生成合法时长可达数分钟，全局 WriteTimeout 会误杀在途 SSE）。
		IdleTimeout: 120 * time.Second,
	}
	go func() {
		<-ctx.Done()
		p.Flush() // 信号触发：先落盘再做优雅停机
		// Flush 已把最后一笔状态快照提交给 Redis（fire-and-forget）；store.Close
		// 等 Upstash 在途/排队写排空再关连接——最后一笔镜像必须写完才退出（发现 4）。
		// Noop 的 Close 是空操作；单写上限 5s × 上限 8，Close 内部另有超时兜底。
		if cErr := store.Close(); cErr != nil {
			log.Printf("WARN: [server] redisstore close: %v", cErr)
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	if cfg.Global.Enabled {
		log.Printf("global realm 已启用（chat_base=%q billing_base=%q，空=默认 workbuddy.ai）",
			cfg.Global.ChatBase, cfg.Global.BillingBase)
	} else {
		log.Printf("global realm 已禁用（config global.enabled=false，纯 CN）")
	}
	// 先显式绑定端口，再打印状态与管理密码：横幅必须在"服务真的能接连接"之后出现，
	// 否则端口被占用导致启动失败时，日志里会留下一个看起来可用的密码。
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		log.Fatalf("listen %s: %v", cfg.Listen, err)
	}
	log.Printf("配置来源: %s", cfg.SourcePath)
	if keys := cfg.LegacyKeys(); len(keys) > 0 {
		log.Printf("WARN: 配置里用了已弃用的键（%s），请改用新键名；旧键后续版本会移除",
			strings.Join(keys, ", "))
	}
	log.Printf("扫码二维码有效期：%v（qrcode_ttl）", cfg.QRCodeTTLDur)
	logScheduleMigration(&cfg.Schedule)
	log.Printf("wp listening on %s (api_key=%v)", ln.Addr(), cfg.APIKey != "")
	if cfg.Admin.Enabled {
		printAdminBanner(adminStore, ln.Addr().String())
	} else {
		log.Printf("管理后台未启用（config admin.enabled=false）：/admin/* 不注册，/ 仍可打开页面但没有数据接口")
	}
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http: %v", err)
	}
	log.Printf("bye")
}

// newLoginManager 构造扫码登录服务并按配置设定二维码有效期（qrcode_ttl）。
//
// TTL 决定"二维码还能扫多久"：到点后 loginsvc 会把它判为过期（ErrStateExpired），
// 管理后台据此显示倒计时、到期把二维码置为"已失效"。
func newLoginManager(ttl time.Duration) *loginsvc.Manager {
	m := loginsvc.New()
	if ttl > 0 {
		m.SetTTL(ttl)
	}
	return m
}
