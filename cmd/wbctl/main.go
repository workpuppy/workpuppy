// Command wbctl 是 workbuddy2api 管理面的命令行客户端。
//
// 为什么有它：轻量版刻意不做 Web 界面，管理动作就落在两种人手上 ——
// 脚本（curl / CI）与运维（命令行）。wbctl 是后者的入口：读同一份 config.json，
// 拿管理密码（服务启动日志里那串），打同一组 /admin/* 端点。它**不含任何业务逻辑**，只是一层
// 便于人读的封装；真正的判定全在服务端（这样 curl 与 wbctl 的行为不可能不一致）。
//
// 用法：
//
//	wbctl accounts                       # 账号清单
//	wbctl checkin                        # 全部签到
//	wbctl checkin 12345678               # 单号签到（顺带回填积分）
//	wbctl login --realm cn               # 扫码加号（打印二维码链接并等授权）
//	wbctl models                         # 模型中心（上下文/输出/档位/积分倍率）
//	wbctl chat --model glm-5.2 "你好"     # 聊天测试台（--uid 可指定账号，--stream 流式）
//	wbctl disable 12345678 --reason 维护  # 摘除对话流量（签到保活照常）
//	wbctl enable 12345678                # 恢复
//	wbctl revive 12345678                # 解除系统自动禁用
//
// 配置来源优先级：-server/-key 命令行 > config.json（-config，默认 ./config.json）。
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// cfgFile 服务端配置（本工具只需要监听地址；管理面凭据不在这份文件里）。
type cfgFile struct {
	Listen string `json:"listen"`
}

// client 管理面客户端。
//
// 凭据是**管理密码**（服务启动时随机生成、打印在容器日志里），不是 /v1 的 api_key：
// 两者已彻底分离，拿 api_key 调 /admin/* 会被 401。密码来源：-password > WB2A_ADMIN_PASSWORD。
type client struct {
	base     string
	password string
	http     *http.Client
}

func main() {
	var (
		cfgPath  = flag.String("config", "config.json", "服务端配置文件（读 listen）")
		server   = flag.String("server", "", "覆盖服务地址，如 http://127.0.0.1:7863")
		password = flag.String("password", "", "管理密码（默认取环境变量 WB2A_ADMIN_PASSWORD）")
		asJSON   = flag.Bool("json", false, "输出原始 JSON")
	)
	flag.Usage = usage
	flag.Parse()

	args := flag.Args()
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}

	c := newClient(*cfgPath, *server, *password)
	var err error
	switch args[0] {
	case "accounts":
		err = c.cmdAccounts(*asJSON)
	case "account":
		if len(args) < 2 {
			fatalf("account 需要 uid")
		}
		err = c.cmdAccount(args[1], *asJSON)
	case "checkin":
		uid := ""
		if len(args) > 1 {
			uid = args[1]
		}
		err = c.cmdCheckin(uid)
	case "login":
		err = c.cmdLogin(args[1:])
	case "models":
		err = c.cmdModels(*asJSON)
	case "chat":
		err = c.cmdChat(args[1:])
	case "disable":
		if len(args) < 2 {
			fatalf("disable 需要 uid")
		}
		err = c.cmdManual(args[1], "disable", flagReason(args[2:]))
	case "enable":
		if len(args) < 2 {
			fatalf("enable 需要 uid")
		}
		err = c.cmdManual(args[1], "enable", "")
	case "revive":
		if len(args) < 2 {
			fatalf("revive 需要 uid")
		}
		err = c.cmdManual(args[1], "revive", "")
	case "help", "-h", "--help":
		usage()
	default:
		fatalf("未知命令 %q（wbctl help 看用法）", args[0])
	}
	if err != nil {
		fatalf("%v", err)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `wbctl —— workbuddy2api 管理面命令行

用法: wbctl [-config config.json] [-server URL] [-password PW] [-json] <命令> [参数]

命令:
  accounts                         账号清单（池状态 + 凭证元信息）
  account <uid>                    单账号详情
  checkin [uid]                    签到（省略 uid = 全部；顺带回填积分）
  login [--realm cn|global] [--wait 300s]
                                   扫码加号：打印二维码链接并等授权，成功自动落盘
  models                           模型中心（上下文 / 最大输出 / 思考档位 / 积分倍率）
  chat --model <m> [--uid <u>] [--stream] [--effort low|medium|high] <prompt>
                                   聊天测试台（复用 /v1 全链路，可指定账号定点验证）
  disable <uid> [--reason 文本]    摘除对话流量（签到与保活照常）
  enable <uid>                     解除手动停用
  revive <uid>                     解除系统自动禁用（封号救不回）

前置条件：服务端 config.json 里 admin.enabled=true；密码取服务启动日志里的「登录密码」，
           也可用 -password 或环境变量 WB2A_ADMIN_PASSWORD 直接给。
`)
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "wbctl: "+format+"\n", args...)
	os.Exit(1)
}

// newClient 解析服务地址与密钥。listen 常写成 ":7863"，这里补成本机地址。
func newClient(cfgPath, server, password string) *client {
	var cf cfgFile
	if raw, err := os.ReadFile(cfgPath); err == nil {
		// 容忍 BOM：Windows 上手工编辑 config.json 很容易带上 BOM，而 json 包会因此
		// 解析失败 —— 那种失败在这里会静默退化成「用默认地址，连不上」。
		raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})
		_ = json.Unmarshal(raw, &cf)
	}
	if server == "" {
		listen := strings.TrimSpace(cf.Listen)
		if listen == "" {
			listen = ":7863"
		}
		if strings.HasPrefix(listen, ":") {
			listen = "127.0.0.1" + listen
		}
		server = "http://" + listen
	}
	if password == "" {
		password = strings.TrimSpace(os.Getenv("WB2A_ADMIN_PASSWORD"))
	}
	return &client{
		base:     strings.TrimRight(server, "/"),
		password: password,
		http:     &http.Client{Timeout: 10 * time.Minute},
	}
}

// do 发一次管理面请求。admin 端点是带 JSON 信封的，非 2xx 一律把信封内容抛出来
// （错误原文就是排障线索，不能吞掉换成"请求失败"）。
func (c *client) do(method, path string, body []byte) ([]byte, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, c.base+path, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.password != "" {
		req.Header.Set("Authorization", "Bearer "+c.password)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("连不上服务端 %s：%w", c.base, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return raw, nil
}

type accountRow struct {
	UID           string  `json:"uid"`
	Nickname      string  `json:"nickname"`
	Realm         string  `json:"realm"`
	Credits       int64   `json:"credits"`
	Cooling       bool    `json:"cooling"`
	CoolRemaining int64   `json:"cool_remaining_sec"`
	Disabled      bool    `json:"disabled"`
	ManualOff     bool    `json:"manual_disabled"`
	InFlight      int     `json:"in_flight"`
	InPool        bool    `json:"in_pool"`
	Checkinable   bool    `json:"checkinable"`
	HasCredential bool    `json:"has_credential"`
	CanRefresh    bool    `json:"can_refresh"`
	ExpiresAt     int64   `json:"expires_at"`
	RemainingSec  int64   `json:"remaining_sec"`
	ProxyName     string  `json:"proxy"`
	Weight        float64 `json:"weight"`
	SuccessCount  int64   `json:"success_count"`
	ErrTotal      int64   `json:"err_total"`
}

func (c *client) cmdAccounts(asJSON bool) error {
	raw, err := c.do(http.MethodGet, "/admin/accounts", nil)
	if err != nil {
		return err
	}
	if asJSON {
		fmt.Println(string(raw))
		return nil
	}
	var resp struct {
		Accounts []accountRow   `json:"accounts"`
		Total    int            `json:"total"`
		Counts   map[string]int `json:"counts"`
		AuthDir  string         `json:"auth_dir"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return err
	}
	fmt.Printf("账号 %d 个（健康 %d / 冷却 %d / 禁用 %d）  目录 %s\n\n",
		resp.Total, resp.Counts["healthy"], resp.Counts["cooling"], resp.Counts["disabled"], resp.AuthDir)
	fmt.Printf("%-12s %-14s %-7s %8s %6s %-12s %-6s %s\n",
		"UID", "昵称", "版本", "积分", "在途", "状态", "权重", "凭证")
	for _, a := range resp.Accounts {
		fmt.Printf("%-12s %-14s %-7s %8d %6d %-12s %-6s %s\n",
			a.UID, truncate(a.Nickname, 14), a.Realm, a.Credits, a.InFlight,
			statusLabel(a), weightLabel(a.Weight), credentialLabel(a))
	}
	return nil
}

func statusLabel(a accountRow) string {
	switch {
	case a.ManualOff:
		return "已手动停用"
	case a.Disabled:
		return "系统禁用"
	case a.Cooling:
		return fmt.Sprintf("冷却 %ds", a.CoolRemaining)
	case !a.InPool:
		return "未入池"
	default:
		return "在线"
	}
}

func weightLabel(w float64) string {
	if w == 0 || w == 1 {
		return "-"
	}
	return fmt.Sprintf("%.2f", w)
}

func credentialLabel(a accountRow) string {
	if !a.HasCredential {
		return "无 accessToken"
	}
	if !a.CanRefresh {
		return "仅 accessToken"
	}
	switch {
	case a.RemainingSec <= 0:
		return "已过期（可刷新）"
	case a.RemainingSec < 48*3600:
		return fmt.Sprintf("%dh 后过期", a.RemainingSec/3600)
	case a.RemainingSec < 60*86400:
		return fmt.Sprintf("%d 天后过期", a.RemainingSec/86400)
	default:
		// 超过两个月只给日期：这个量级下「多少天后」已经没有决策价值。
		return time.Unix(a.ExpiresAt, 0).Format("2006-01-02") + " 过期"
	}
}

func (c *client) cmdAccount(uid string, asJSON bool) error {
	raw, err := c.do(http.MethodGet, "/admin/accounts/"+uid, nil)
	if err != nil {
		return err
	}
	if asJSON {
		fmt.Println(string(raw))
		return nil
	}
	var v map[string]any
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	pretty, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(pretty))
	return nil
}

func (c *client) cmdCheckin(uid string) error {
	path := "/admin/accounts/checkin"
	if uid != "" {
		path = "/admin/accounts/" + uid + "/checkin"
	}
	raw, err := c.do(http.MethodPost, path, []byte("{}"))
	if err != nil {
		return err
	}
	var resp map[string]any
	if err := json.Unmarshal(raw, &resp); err != nil {
		return err
	}
	if uid != "" {
		// 单号：status=ok|already|fail（fail 已由 HTTP 错误路径处理）
		status, _ := resp["status"].(string)
		credits, hasCredits := resp["credits"].(float64)
		if hasCredits {
			fmt.Printf("%s 签到 %s，积分 %.0f\n", uid, status, credits)
		} else {
			fmt.Printf("%s 签到 %s\n", uid, status)
		}
		return nil
	}
	fmt.Printf("批量签到：成功 %v / 已签 %v / 失败 %v / 跳过 %v（共 %v）\n",
		resp["ok"], resp["already"], resp["fail"], resp["skipped"], resp["total"])
	if outs, ok := resp["outcomes"].([]any); ok {
		for _, o := range outs {
			m, _ := o.(map[string]any)
			st, _ := m["status"].(string)
			if st == "ok" || st == "fail" {
				fmt.Printf("  %-12v %-8s %v\n", m["uid"], st, m["detail"])
			}
		}
	}
	return nil
}

func (c *client) cmdModels(asJSON bool) error {
	raw, err := c.do(http.MethodGet, "/admin/models", nil)
	if err != nil {
		return err
	}
	if asJSON {
		fmt.Println(string(raw))
		return nil
	}
	var resp struct {
		Source string           `json:"source"`
		Count  int              `json:"count"`
		Data   []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return err
	}
	fmt.Printf("模型 %d 个（来源 %s）\n\n", resp.Count, resp.Source)
	fmt.Printf("%-28s %10s %10s %-22s %-9s %s\n", "ID", "上下文", "最大输出", "思考档位", "积分倍率", "描述")
	for _, m := range resp.Data {
		id, _ := m["id"].(string)
		desc, _ := m["description"].(string)
		credits, _ := m["credits"].(string)
		efforts := effortLabel(m)
		fmt.Printf("%-28s %10d %10d %-22s %-9s %s\n",
			id, numOf(m, "context_window"), numOf(m, "max_tokens"), efforts, credits, truncate(desc, 30))
	}
	return nil
}

// effortLabel 把 reasoning_supported_efforts 渲染成 "low/medium/high"。
func effortLabel(m map[string]any) string {
	raw, ok := m["reasoning_supported_efforts"].([]any)
	if !ok || len(raw) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			parts = append(parts, s)
		}
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, "/")
}

func numOf(m map[string]any, key string) int64 {
	if v, ok := m[key].(float64); ok {
		return int64(v)
	}
	return 0
}

func (c *client) cmdChat(args []string) error {
	fs := flag.NewFlagSet("chat", flag.ExitOnError)
	model := fs.String("model", "", "模型 ID（wbctl models 可查）")
	uid := fs.String("uid", "", "指定账号（定点验证某个号）")
	stream := fs.Bool("stream", false, "流式输出")
	effort := fs.String("effort", "", "reasoning_effort：low|medium|high（按模型支持档位）")
	_ = fs.Parse(args)
	prompt := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if *model == "" || prompt == "" {
		return fmt.Errorf("用法: wbctl chat --model <模型> [--uid <账号>] [--stream] [--effort low] <提示词>")
	}
	body := map[string]any{
		"model":    *model,
		"messages": []map[string]any{{"role": "user", "content": prompt}},
		"stream":   *stream,
	}
	if *effort != "" {
		body["reasoning_effort"] = *effort
	}
	payload, _ := json.Marshal(body)
	path := "/admin/playground/chat"
	if *uid != "" {
		path += "?uid=" + *uid
	}

	req, err := http.NewRequest(http.MethodPost, c.base+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.password != "" {
		req.Header.Set("Authorization", "Bearer "+c.password)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("连不上服务端：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if *stream {
		// 流式：原样吐 SSE（每行 "data: {...}"），交给调用方 grep/管道处理。
		_, err := io.Copy(os.Stdout, resp.Body)
		return err
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	var out struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		fmt.Println(string(raw))
		return nil
	}
	if len(out.Choices) == 0 {
		fmt.Println(string(raw))
		return nil
	}
	if rc := out.Choices[0].Message.ReasoningContent; rc != "" {
		fmt.Printf("--- 思考 ---\n%s\n", rc)
	}
	fmt.Printf("--- 回答（%s）---\n%s\n", out.Model, out.Choices[0].Message.Content)
	if len(out.Usage) > 0 {
		fmt.Printf("--- 用量 ---\n%v\n", out.Usage)
	}
	return nil
}

func (c *client) cmdLogin(args []string) error {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	realm := fs.String("realm", "cn", "cn|global")
	wait := fs.Duration("wait", 5*time.Minute, "等待授权的最长时间")
	_ = fs.Parse(args)

	body, _ := json.Marshal(map[string]string{"realm": *realm})
	raw, err := c.do(http.MethodPost, "/admin/auth/start", body)
	if err != nil {
		return err
	}
	var start struct {
		State   string `json:"state"`
		AuthURL string `json:"auth_url"`
		Realm   string `json:"realm"`
	}
	if err := json.Unmarshal(raw, &start); err != nil {
		return err
	}
	fmt.Printf("扫码登录（%s）：\n  %s\n\n用微信/QQ 扫码，或直接在浏览器打开上面的链接。等待授权…\n", start.Realm, start.AuthURL)

	deadline := time.Now().Add(*wait)
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		path := fmt.Sprintf("/admin/auth/poll?state=%s&realm=%s", start.State, start.Realm)
		pollRaw, err := c.do(http.MethodGet, path, nil)
		if err != nil {
			return err
		}
		var res struct {
			Status   string `json:"status"`
			UID      string `json:"uid"`
			Nickname string `json:"nickname"`
			Realm    string `json:"realm"`
			File     string `json:"file"`
		}
		if err := json.Unmarshal(pollRaw, &res); err != nil {
			return err
		}
		if res.Status == "saved" {
			fmt.Printf("已添加：%s（%s，%s）\n  文件：%s\n", res.UID, res.Nickname, res.Realm, res.File)
			return nil
		}
	}
	return fmt.Errorf("等待授权超时（%s）；重新执行 wbctl login 获取新二维码", *wait)
}

func (c *client) cmdManual(uid, action, reason string) error {
	body := []byte("{}")
	if reason != "" {
		b, _ := json.Marshal(map[string]string{"reason": reason})
		body = b
	}
	raw, err := c.do(http.MethodPost, "/admin/accounts/"+uid+"/"+action, body)
	if err != nil {
		return err
	}
	var res struct {
		UID          string `json:"uid"`
		ManualOff    bool   `json:"manual_disabled"`
		ManualReason string `json:"manual_reason"`
		Disabled     bool   `json:"disabled"`
		Changed      bool   `json:"changed"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return err
	}
	fmt.Printf("%s：手动停用=%v（%s） 系统禁用=%v 本次变更=%v\n",
		res.UID, res.ManualOff, orNone(res.ManualReason), res.Disabled, res.Changed)
	return nil
}

func orNone(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// flagReason 从剩余参数里取 --reason 的值（disable 子命令用；flag 包在这里不便复用）。
func flagReason(args []string) string {
	for i := 0; i < len(args); i++ {
		if args[i] == "--reason" && i+1 < len(args) {
			return args[i+1]
		}
		if strings.HasPrefix(args[i], "--reason=") {
			return strings.TrimPrefix(args[i], "--reason=")
		}
	}
	return ""
}
