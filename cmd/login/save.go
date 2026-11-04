// save.go login.sh 的「落盘 + 首次签到 + 国际版地区/trial」段（原为内嵌 Python）。
//
// 为什么搬到 Go：镜像不再安装 python3（目标形态是单镜像 + Go 二进制），而这三段
// 逻辑是登录流程里最容易出错的部分——权限、原子写、幂等码判定，散在 shell heredoc
// 里既无法测试也无法复用。现在它们是 `login save` 一个子命令，shell 只负责交互与
// 重启容器。
//
// 输入约定：`login --realm=<cn|global> poll` 的 JSON 从 stdin 传入（shell 里是
// `echo "$RESULT" | login save`）。输出是人读的进度行 + 结尾横幅。
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/region"
	"workbuddy2api/internal/upstream"
)

// saveInput poll 输出的字段（键名与 buildLoginOutput 一致）。
type saveInput struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	Domain       string `json:"domain"`
	Realm        string `json:"realm"`
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterprise_id"`
	Nickname     string `json:"nickname"`
}

// containerUID 容器内 app 的运行 uid（Dockerfile USER 10001）。
// 宿主机以别的 uid 落盘时，容器内读不到 → 账号数为 0（issue #108），必须提示。
const containerUID = 10001

// runSave 执行 `login save`：解析 stdin 的凭据 → 首次签到（CN）→ 原子落盘 →
// 国际版地区完善 + trial（global）。任何一步失败都不撤回落盘结果。
func runSave(realm, regionCode string) {
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	if err != nil || len(strings.TrimSpace(string(raw))) == 0 {
		fatal("save: 需要从 stdin 传入 poll 的 JSON")
	}
	var in saveInput
	if err := json.Unmarshal(raw, &in); err != nil {
		fatal("save: 解析 poll JSON 失败: %v", err)
	}
	if strings.TrimSpace(in.UID) == "" || strings.TrimSpace(in.AccessToken) == "" {
		fatal("save: 凭据不完整（缺 uid 或 access_token），请确认登录已完成")
	}
	if in.Realm == "" {
		in.Realm = auth.ResolveRealm(realm, in.Domain)
	}

	expiresAt := time.Now().Add(time.Duration(expiresInOrDefault(in.ExpiresIn)) * time.Second).Unix()
	a := auth.NewFromLogin(in.UID, in.EnterpriseID, in.Nickname,
		in.AccessToken, in.RefreshToken, in.Domain, in.Realm, expiresAt)

	// 1) 首次签到（仅 CN）。
	//    为什么 global 跳过：国际版计费/签到端点未实测，误打 CN 端点只会制造假失败。
	if a.Realm() == "global" {
		fmt.Println("签到: global realm 跳过（国际版签到端点未实测）")
	} else {
		up := upstream.New()
		if err := up.DailyCheckin(a); err != nil {
			if upstream.IsAlreadyCheckin(err) {
				fmt.Println("签到: 今天已签到（幂等）")
			} else {
				fmt.Printf("签到: %v\n", err)
			}
		} else {
			fmt.Println("签到: 成功")
		}
	}

	// 2) 落盘（原子替换，与 internal/auth 读取格式一致）。
	dir := authDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("save: 无法创建账号目录 %s: %v", dir, err)
	}
	file := filepath.Join(dir, "workbuddy-"+in.UID+".json")
	action := "新增"
	if _, err := os.Stat(file); err == nil {
		action = "覆盖"
		fmt.Printf("账号已存在（uid=%s），将覆盖更新凭证\n", in.UID)
	} else {
		fmt.Printf("新账号（uid=%s），新增 auth 文件\n", in.UID)
	}
	a.FilePath = file
	if err := a.SaveAtomic(); err != nil {
		fmt.Fprintf(os.Stderr, "\n❌ 写入失败：%v\n", err)
		if !dirWritable(dir) {
			fmt.Fprintf(os.Stderr, "    目录 %s 不可写（权限不足）\n", dir)
			fmt.Fprintf(os.Stderr, "    请执行：chown -R %d:%d %s\n", containerUID, containerUID, dir)
			fmt.Fprintf(os.Stderr, "    或在容器内登录：docker compose exec -it wb2api bash -c './login.sh'\n\n")
		}
		os.Exit(1)
	}
	if uid, ok := fileUID(file); ok && uid != containerUID {
		fmt.Printf("\n⚠️  权限警告：%s 属主 uid=%d，容器内 app(uid=%d) 可能读不到\n", file, uid, containerUID)
		fmt.Printf("    请执行：chown -R %d:%d ./auths\n", containerUID, containerUID)
		fmt.Printf("    或在容器内登录（属主自动正确）：docker compose exec -it wb2api bash -c './login.sh'\n\n")
	}
	fmt.Printf("已保存（%s）: %s\n", action, file)

	// 3) 国际版：注册激活 → 缺地区则完善 → trial 领取（全部幂等，失败不阻断登录）。
	if a.Realm() == "global" {
		runGlobalProvision(a, regionCode)
	}

	// 4) 结尾横幅（原来由 shell 打印，参数散在 shell 变量里；现在统一在这里给出）。
	fmt.Println()
	fmt.Println("============================================================")
	fmt.Println("  登录完成！")
	fmt.Printf("  UID: %s\n", a.UID)
	fmt.Printf("  Realm: %s\n", a.Realm())
	if a.Nickname != "" {
		fmt.Printf("  Nickname: %s\n", a.Nickname)
	} else {
		fmt.Println("  Nickname: （未获取到）")
	}
	fmt.Printf("  Token: %s...\n", prefix(a.AccessTokenValue(), 30))
	fmt.Printf("  有效期: %s\n", time.Unix(expiresAt, 0).Format("2006-01-02 15:04"))
	fmt.Println("============================================================")
}

// runGlobalProvision 国际版注册地区完善 + trial 领取。
//
// 背景：新 global 账号需先完善注册地区再调 register 激活，chat 才不报 14017；
// 地区完善走 region 包（拉列表 → 检测当前地区 → 菜单/直选 → 提交 → 复验）。
func runGlobalProvision(a *auth.Auth, regionCode string) {
	token := a.AccessTokenValue()
	res := region.Activate(region.Base, token, a.UID)
	switch {
	case res.OK:
		fmt.Println("注册激活: 成功")
	case res.NeedsRegion:
		fmt.Println("注册激活: 需完善注册地区")
		finishRegion(a, token, regionCode)
	default:
		fmt.Printf("注册激活: %s\n", res.Msg)
	}

	tr := region.ClaimTrial(region.Base, token)
	if tr.OK {
		suffix := ""
		if tr.Already {
			suffix = "（已领取过）"
		}
		fmt.Printf("国际版 trial: 已激活，可以开始对话%s\n", suffix)
	} else {
		fmt.Printf("国际版 trial: %s（不阻断登录）\n", tr.Msg)
	}
}

// finishRegion 让用户选地区并提交（检测到已设置则直接复用，不出菜单）。
func finishRegion(a *auth.Auth, token, regionCode string) {
	countries, err := region.FetchCountries(region.Base, true)
	if err != nil {
		fmt.Printf("  拉取地区列表失败: %v\n", err)
		return
	}
	curIOS2, curName, _ := region.DetectRegion(region.Base, token)
	pick := region.PickByIOS2(countries, regionCode)
	if pick == nil && curIOS2 != "" {
		if p := region.PickByIOS2(countries, curIOS2); p != nil {
			fmt.Printf("  检测到当前地区: %s %s，跳过选项单\n", p.IOS2, p.EnName)
			pick = p
		} else if curName != "" {
			fmt.Printf("  检测到当前地区: %s %s（不在国际版白名单内）\n", curIOS2, curName)
		}
	}
	if pick == nil {
		if regionCode != "" {
			// --region 指定了但不在白名单：退回全量列表再找一次。
			if all, err := region.FetchCountries(region.Base, false); err == nil {
				pick = region.PickByIOS2(all, regionCode)
			}
			if pick == nil {
				fmt.Printf("  未找到地区 %s，跳过完善\n", regionCode)
				return
			}
		} else {
			pick = region.InteractiveMenu(countries, curIOS2, "国际版账号需完善注册地区")
			if pick == nil {
				fmt.Println("  未选择地区，跳过完善（后续可重跑登录完善）")
				return
			}
		}
	}
	if err := region.CompleteFlow(region.Base, token, a.UID, pick, false); err != nil {
		fmt.Printf("  地区完善失败: %v\n", err)
		return
	}
	fmt.Printf("  地区已完善: %s %s（register 重新验证通过）\n", pick.IOS2, pick.EnName)
}

// runAccounts 打印当前账号数（login.sh 重启容器后的收尾检查）。
// 读 config.json 的 api_key 打 /status，与网关自身口径一致。
func runAccounts() {
	key, addr := readConfigKeyAndAddr()
	url := strings.TrimRight(addr, "/") + "/status"
	if v := strings.TrimSpace(os.Getenv("WB2A_STATUS_URL")); v != "" {
		url = v
	}
	n, err := statusAccountCount(url, key)
	if err != nil {
		fmt.Println("?")
		return
	}
	fmt.Println(n)
}

func expiresInOrDefault(v int64) int64 {
	if v <= 0 {
		// 上游没给有效期时按 1 小时处理：写 0 会让 token 一出生就被判过期。
		return 3600
	}
	return v
}

// authDir 账号目录：优先 WB2A_AUTH_DIR，其次 WB2A_LOGIN_AUTH_DIR，最后 ./auths。
func authDir() string {
	for _, k := range []string{"WB2A_AUTH_DIR", "WB2A_LOGIN_AUTH_DIR"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return "./auths"
}

func dirWritable(dir string) bool {
	f, err := os.CreateTemp(dir, ".wb-probe-*")
	if err != nil {
		return false
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	return true
}

func prefix(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// readConfigKeyAndAddr 读 config.json 的 api_key 与 listen（BOM 容忍）。
func readConfigKeyAndAddr() (key, addr string) {
	path := strings.TrimSpace(os.Getenv("WB2A_CONFIG"))
	if path == "" {
		path = "config.json"
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", "http://127.0.0.1:7863"
	}
	raw = trimBOM(raw)
	var cf struct {
		Listen string `json:"listen"`
		APIKey string `json:"api_key"`
	}
	_ = json.Unmarshal(raw, &cf)
	listen := strings.TrimSpace(cf.Listen)
	if listen == "" {
		listen = ":7863"
	}
	if strings.HasPrefix(listen, ":") {
		listen = "127.0.0.1" + listen
	}
	return cf.APIKey, "http://" + listen
}
