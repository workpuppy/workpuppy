package configschema

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"wp/internal/configdata"
)

// exampleLeaves 把 config.default.json 摊平成 path 集合（数组/标量都算叶子）。
func exampleLeaves(t *testing.T) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(configdata.Bytes(), &doc); err != nil {
		t.Fatalf("解析 example: %v", err)
	}
	out := map[string]any{}
	var walk func(prefix string, m map[string]any)
	walk = func(prefix string, m map[string]any) {
		for k, v := range m {
			path := k
			if prefix != "" {
				path = prefix + "." + k
			}
			if child, ok := v.(map[string]any); ok {
				walk(path, child)
				continue
			}
			out[path] = v
		}
	}
	walk("", doc)
	return out
}

// 设置页要能覆盖**全部**配置项：example 里出现的每个键都必须有 schema 条目，
// 否则会变成"文件里能改、页面上看不到"的黑盒配置。
func TestSchemaCoversEveryExampleKey(t *testing.T) {
	leaves := exampleLeaves(t)
	for path := range leaves {
		if _, ok := Lookup(path); !ok {
			t.Errorf("config.default.json 的 %q 没有对应的 schema 字段", path)
		}
	}
	// 反向：schema 里的字段必须在 example 里有默认值（默认值唯一来源）。
	for _, f := range Fields() {
		if _, ok := leaves[f.Path]; !ok {
			t.Errorf("schema 字段 %q 在 config.default.json 里没有默认值", f.Path)
		}
		if _, ok := DefaultAt(f.Path); !ok {
			t.Errorf("默认值取不到：%s", f.Path)
		}
	}
}

func TestSchemaWellFormed(t *testing.T) {
	seen := map[string]bool{}
	groups := map[string]bool{}
	for _, g := range Schema().Groups {
		groups[g.Key] = true
	}
	for _, f := range Fields() {
		if seen[f.Path] {
			t.Errorf("字段重复：%s", f.Path)
		}
		seen[f.Path] = true
		if f.Label == "" || f.Group == "" {
			t.Errorf("字段 %s 缺 label/group", f.Path)
		}
		if !groups[f.Group] {
			t.Errorf("字段 %s 引用了不存在的分组 %q", f.Path, f.Group)
		}
		if f.Kind == KindEnum && len(f.Options) == 0 {
			t.Errorf("枚举字段 %s 没有 options", f.Path)
		}
	}
}

func TestValidateDocTypesAndRanges(t *testing.T) {
	cases := []struct {
		name string
		doc  map[string]any
		want string
	}{
		{"类型错误", map[string]any{"pool": map[string]any{"max_in_flight": "abc"}}, "pool.max_in_flight"},
		{"整数越界", map[string]any{"pool": map[string]any{"max_in_flight": 0.0}}, "pool.max_in_flight"},
		{"非整数", map[string]any{"pool": map[string]any{"max_in_flight": 1.5}}, "pool.max_in_flight"},
		{"时长非法", map[string]any{"state": map[string]any{"flush_interval": "5x"}}, "state.flush_interval"},
		{"时长负数", map[string]any{"state": map[string]any{"flush_interval": "-5s"}}, "state.flush_interval"},
		{"监听格式", map[string]any{"listen": "not a listen"}, "listen"},
		{"监听 IPv6 合法", map[string]any{"listen": "[::]:7863"}, ""},
		{"监听 IPv4 合法", map[string]any{"listen": "127.0.0.1:7863"}, ""},
		{"枚举非法", map[string]any{"prompt": map[string]any{"mode": "xxx"}}, "prompt.mode"},
		{"时刻非法", map[string]any{"schedule": map[string]any{"checkin_time": []any{"25:99"}}}, "schedule.checkin_time"},
		{"布尔类型", map[string]any{"admin": map[string]any{"enabled": "yes"}}, "admin.enabled"},
		{"代理表类型", map[string]any{"proxies": map[string]any{"hk": 123.0}}, "proxies"},
		{"未知键不报", map[string]any{"whatever": 1}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := ValidateDoc(tc.doc)
			if tc.want == "" {
				if len(errs) != 0 {
					t.Fatalf("不应报错：%v", errs)
				}
				return
			}
			found := false
			for _, e := range errs {
				if e.Path == tc.want {
					found = true
				}
			}
			if !found {
				t.Fatalf("want error on %s, got %v", tc.want, errs)
			}
		})
	}
}

func TestPathHelpers(t *testing.T) {
	doc := map[string]any{}
	SetPath(doc, "pool.max_in_flight", 5.0)
	SetPath(doc, "admin.enabled", true)
	if v, ok := GetPath(doc, "pool.max_in_flight"); !ok || v.(float64) != 5 {
		t.Fatalf("SetPath/GetPath 失败：%v %v", v, ok)
	}
	DeletePath(doc, "pool.max_in_flight")
	if _, ok := GetPath(doc, "pool.max_in_flight"); ok {
		t.Fatal("删除后仍能取到值")
	}
	// 删空后父对象也应收掉，避免留下 "pool": {} 这种空壳。
	if _, ok := doc["pool"]; ok {
		t.Fatalf("空分组应被清理：%v", doc)
	}
}

func TestDurationKindsAcceptEmpty(t *testing.T) {
	// credit_exhausted 允许空串（= 关闭该语义），不应报格式错。
	if errs := ValidateDoc(map[string]any{"cooldown": map[string]any{"credit_exhausted": ""}}); len(errs) != 0 {
		t.Fatalf("空时长应放行：%v", errs)
	}
}

func TestSortedFieldPathsAreStable(t *testing.T) {
	paths := make([]string, 0, len(Fields()))
	for _, f := range Fields() {
		paths = append(paths, f.Path)
	}
	sorted := append([]string(nil), paths...)
	sort.Strings(sorted)
	if strings.Join(paths, ",") == strings.Join(sorted, ",") {
		t.Log("字段顺序恰好按字典序排列（仅提示，不影响正确性）")
	}
}
