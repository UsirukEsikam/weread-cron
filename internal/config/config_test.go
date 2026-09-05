package config

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// validEnv 返回一份配置合法的环境变量集合（后续用例在其上做单点破坏）。
func validEnv() map[string]string {
	return map[string]string{
		EnvCookie:      "wr_skey=abc; wr_gid=123",
		EnvBooks:       "book1, book2",
		EnvWindowStart: "01:00",
		EnvWindowEnd:   "03:00",
		EnvReadMin:     "40",
		EnvReadMax:     "70",
		EnvBarkURL:     "https://api.day.app/xxx/",
		EnvWeComURL:    "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=abc",
		EnvDataDir:     "/data",
		EnvTZ:          "Asia/Shanghai",
	}
}

func lookupFrom(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

func mustLoad(t *testing.T, env map[string]string) *Config {
	t.Helper()
	cfg, err := Load(lookupFrom(env))
	if err != nil {
		t.Fatalf("Load() 意外失败: %v", err)
	}
	return cfg
}

func loadErr(t *testing.T, env map[string]string) error {
	t.Helper()
	_, err := Load(lookupFrom(env))
	if err == nil {
		t.Fatal("Load() 应失败，但成功了")
	}
	return err
}

func TestLoadValid(t *testing.T) {
	cfg := mustLoad(t, validEnv())

	if cfg.Cookie != "wr_skey=abc; wr_gid=123" {
		t.Errorf("Cookie = %q", cfg.Cookie)
	}
	if len(cfg.Books) != 2 || cfg.Books[0] != "book1" || cfg.Books[1] != "book2" {
		t.Errorf("Books = %#v", cfg.Books)
	}
	if cfg.WindowStart != 60 || cfg.WindowEnd != 180 {
		t.Errorf("Window = %d-%d", cfg.WindowStart, cfg.WindowEnd)
	}
	if cfg.WindowString() != "01:00-03:00" {
		t.Errorf("WindowString = %q", cfg.WindowString())
	}
	if cfg.ReadMinutesMin != 40 || cfg.ReadMinutesMax != 70 {
		t.Errorf("ReadMinutes = %d-%d", cfg.ReadMinutesMin, cfg.ReadMinutesMax)
	}
	if cfg.BarkURL == "" || cfg.WeComWebhookURL == "" {
		t.Errorf("通知渠道 URL 丢失: bark=%q wecom=%q", cfg.BarkURL, cfg.WeComWebhookURL)
	}
	if cfg.DataDir != "/data" {
		t.Errorf("DataDir = %q", cfg.DataDir)
	}
	if cfg.TZName != "Asia/Shanghai" || cfg.TZ.String() != "Asia/Shanghai" {
		t.Errorf("TZ = %q / %v", cfg.TZName, cfg.TZ)
	}
}

func TestLoadDefaults(t *testing.T) {
	env := validEnv()
	delete(env, EnvDataDir)
	delete(env, EnvTZ)
	delete(env, EnvCookie)
	delete(env, EnvBooks)
	delete(env, EnvBarkURL)
	delete(env, EnvWeComURL)
	cfg := mustLoad(t, env)

	if cfg.DataDir != DefaultDataDir {
		t.Errorf("DataDir 默认 = %q，期望 %q", cfg.DataDir, DefaultDataDir)
	}
	if cfg.TZName != DefaultTZName {
		t.Errorf("TZName 默认 = %q，期望 %q", cfg.TZName, DefaultTZName)
	}
	if cfg.TZ == nil {
		t.Error("TZ 默认时区未加载")
	}
	if cfg.Cookie != "" || len(cfg.Books) != 0 {
		t.Errorf("可选配置应默认为零值: cookie=%q books=%#v", cfg.Cookie, cfg.Books)
	}
}

func TestWindowStartEqualsEndIsFixedTime(t *testing.T) {
	env := validEnv()
	env[EnvWindowStart] = "02:30"
	env[EnvWindowEnd] = "02:30"
	cfg := mustLoad(t, env)

	if cfg.WindowStart != cfg.WindowEnd {
		t.Errorf("start==end 应合法且表示固定时刻，got %d-%d", cfg.WindowStart, cfg.WindowEnd)
	}
}

func TestBooksEmptyMeansAutoSelect(t *testing.T) {
	// 显式设置为空（目录中常见的 WEREAD_CRON_BOOKS: ""）应视为“未指定书籍”= 自动从 Shelf 选书（User Story 19），
	// 而不是配置错误。
	for _, v := range []string{"", "   "} {
		env := validEnv()
		env[EnvBooks] = v
		cfg := mustLoad(t, env)
		if len(cfg.Books) != 0 {
			t.Errorf("BOOKS=%q 应解析为空列表，got %#v", v, cfg.Books)
		}
	}

	// 列表内部空元素仍是书写错误，明确报错。
	env := validEnv()
	env[EnvBooks] = "book1,,book3"
	err := loadErr(t, env)
	if !strings.Contains(err.Error(), EnvBooks) || !strings.Contains(err.Error(), "第 2 项为空") {
		t.Errorf("内部空元素应报错；实际: %s", err.Error())
	}
}

func TestLoadValidation(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(map[string]string)
		wantErr []string // stderr 文案中必须包含的违规项子串
	}{
		{
			name: "缺失全部必需变量",
			mutate: func(m map[string]string) {
				for _, k := range []string{EnvWindowStart, EnvWindowEnd, EnvReadMin, EnvReadMax} {
					delete(m, k)
				}
			},
			wantErr: []string{
				EnvWindowStart + " 未设置",
				EnvWindowEnd + " 未设置",
				EnvReadMin + " 未设置",
				EnvReadMax + " 未设置",
			},
		},
		{
			name:    "窗口 start 晚于 end 且不相等",
			mutate:  func(m map[string]string) { m[EnvWindowStart], m[EnvWindowEnd] = "03:00", "01:00" },
			wantErr: []string{"运行窗口", EnvWindowStart, EnvWindowEnd, "start==end"},
		},
		{
			name:    "窗口时间格式非法-小时越界",
			mutate:  func(m map[string]string) { m[EnvWindowStart] = "25:00" },
			wantErr: []string{EnvWindowStart, "时间格式非法", "HH:MM"},
		},
		{
			name:    "窗口时间格式非法-分钟越界",
			mutate:  func(m map[string]string) { m[EnvWindowEnd] = "01:60" },
			wantErr: []string{EnvWindowEnd, "时间格式非法", "HH:MM"},
		},
		{
			name:    "窗口时间格式非法-含秒",
			mutate:  func(m map[string]string) { m[EnvWindowStart] = "01:00:00" },
			wantErr: []string{EnvWindowStart, "时间格式非法", "HH:MM"},
		},
		{
			name:    "时长 min 大于 max",
			mutate:  func(m map[string]string) { m[EnvReadMin], m[EnvReadMax] = "90", "45" },
			wantErr: []string{EnvReadMin, EnvReadMax, "大于"},
		},
		{
			name:    "时长非整数",
			mutate:  func(m map[string]string) { m[EnvReadMin] = "abc" },
			wantErr: []string{EnvReadMin, "不是有效整数"},
		},
		{
			name:    "时长为零",
			mutate:  func(m map[string]string) { m[EnvReadMin] = "0" },
			wantErr: []string{EnvReadMin, "必须大于 0"},
		},
		{
			name:    "时长为负",
			mutate:  func(m map[string]string) { m[EnvReadMax] = "-5" },
			wantErr: []string{EnvReadMax, "必须大于 0"},
		},
		{
			name:    "时区非法",
			mutate:  func(m map[string]string) { m[EnvTZ] = "Non/Existent" },
			wantErr: []string{EnvTZ, "无法识别的时区"},
		},
		{
			name:    "BOOKS 含空元素",
			mutate:  func(m map[string]string) { m[EnvBooks] = "book1,,book3" },
			wantErr: []string{EnvBooks, "第 2 项为空"},
		},
		{
			name:    "Cookie 只含空白",
			mutate:  func(m map[string]string) { m[EnvCookie] = "   " },
			wantErr: []string{EnvCookie, "只含空白字符"},
		},
		{
			name:    "Bark URL 非法",
			mutate:  func(m map[string]string) { m[EnvBarkURL] = "not-a-url" },
			wantErr: []string{EnvBarkURL, "不是有效的 http(s) URL"},
		},
		{
			name:    "企业微信 URL 协议非法",
			mutate:  func(m map[string]string) { m[EnvWeComURL] = "ftp://qyapi.weixin.qq.com/x" },
			wantErr: []string{EnvWeComURL, "不是有效的 http(s) URL"},
		},
		{
			name:    "DataDir 只含空白",
			mutate:  func(m map[string]string) { m[EnvDataDir] = "  " },
			wantErr: []string{EnvDataDir, "只含空白字符"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := validEnv()
			tc.mutate(env)
			err := loadErr(t, env)

			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("错误类型应为 *ValidationError，got %T: %v", err, err)
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("违规信息缺少 %q；实际:\n%s", want, err.Error())
				}
			}
			// 每个问题独立成行（stderr 可逐项定位）
			if len(ve.Problems) == 0 {
				t.Error("Problems 为空")
			}
		})
	}
}

func TestMultipleProblemsReported(t *testing.T) {
	env := validEnv()
	env[EnvWindowStart] = "25:00" // 时间格式
	env[EnvReadMax] = "abc"       // 非整数
	env[EnvTZ] = "Foo/Bar"        // 时区
	err := loadErr(t, env)

	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("错误类型应为 *ValidationError，got %T", err)
	}
	if len(ve.Problems) != 3 {
		t.Errorf("应同时报告 3 个违规项，got %d: %#v", len(ve.Problems), ve.Problems)
	}
}

func TestCustomTZAndWindowBoundary(t *testing.T) {
	env := validEnv()
	env[EnvTZ] = "America/New_York"
	env[EnvWindowStart] = "00:00"
	env[EnvWindowEnd] = "23:59"
	cfg := mustLoad(t, env)

	if cfg.TZName != "America/New_York" {
		t.Errorf("TZName = %q", cfg.TZName)
	}
	// 窗口边界合法：跨整天 start<end 且格式正确
	if cfg.WindowStart != 0 || cfg.WindowEnd != 23*60+59 {
		t.Errorf("Window = %d-%d", cfg.WindowStart, cfg.WindowEnd)
	}
}

func TestTZEmptyMeansDefault(t *testing.T) {
	env := validEnv()
	env[EnvTZ] = ""
	cfg := mustLoad(t, env)
	if cfg.TZName != DefaultTZName {
		t.Errorf("空 TZ 应回退默认 %q，got %q", DefaultTZName, cfg.TZName)
	}
}

func TestLoadLocationUsesConfigTZ(t *testing.T) {
	env := validEnv()
	env[EnvTZ] = "Asia/Shanghai"
	cfg := mustLoad(t, env)

	// 验证时区确实被加载且能用于时间计算（ADR-0003 语义）
	loc := time.FixedZone("Asia/Shanghai", 8*3600)
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, loc)
	got := now.In(cfg.TZ)
	if got.Hour() != 0 {
		t.Errorf("时区换算错误: %v", got)
	}
}
