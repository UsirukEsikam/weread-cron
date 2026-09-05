// Package config 负责从环境变量解析与校验用户可见配置（WEREAD_CRON_* 前缀，ADR-0005）。
// 校验失败即启动失败：窗口 start>end 且不相等非法、时长 min>max 非法、时间/时区格式非法。
package config

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// 用户可见环境变量（全部带 WEREAD_CRON_ 前缀，见 ADR-0005）。
const (
	EnvCookie        = "WEREAD_CRON_COOKIE"
	EnvBooks         = "WEREAD_CRON_BOOKS"
	EnvWindowStart   = "WEREAD_CRON_RUN_WINDOW_START"
	EnvWindowEnd     = "WEREAD_CRON_RUN_WINDOW_END"
	EnvReadMin       = "WEREAD_CRON_READ_MINUTES_MIN"
	EnvReadMax       = "WEREAD_CRON_READ_MINUTES_MAX"
	EnvBarkURL       = "WEREAD_CRON_BARK_URL"
	EnvWeComURL      = "WEREAD_CRON_WECOM_WEBHOOK_URL"
	EnvDataDir       = "WEREAD_CRON_DATA_DIR"
	EnvTZ            = "TZ"
	DefaultDataDir   = "/data"
	DefaultTZName    = "Asia/Shanghai"
	windowTimeLayout = "15:04"
)

// Config 是解析并校验后的全部用户可见配置。
type Config struct {
	// Cookie 是初始 Cookie header 字符串（可选；无持久化 Login Session 时必需）。
	Cookie string
	// Books 是候选 bookId 列表（逗号分隔），可为空 = 自动从 Shelf 选书。
	Books []string
	// WindowStart/WindowEnd 是 Run Window 的分钟数（自当天 00:00 起），只约束 Task 开始时间（ADR-0001）。
	// WindowStart == WindowEnd 表示固定启动时刻（合法）。
	WindowStart int
	WindowEnd   int
	// ReadMinutesMin/ReadMinutesMax 是 Target Duration 的随机范围（分钟）。
	ReadMinutesMin int
	ReadMinutesMax int
	// BarkURL / WeComWebhookURL 为通知渠道，可为空（按配置独立启用）。
	BarkURL         string
	WeComWebhookURL string
	// DataDir 是持久化目录（Login Session、Terminal State），默认 /data。
	DataDir string
	// TZ 决定一切时间语义（ADR-0003 内嵌 tzdata），默认 Asia/Shanghai。
	TZ     *time.Location
	TZName string
}

// WindowString 返回 HH:MM-HH:MM 形式的 Run Window 描述，仅用于日志/输出。
func (c *Config) WindowString() string {
	return fmt.Sprintf("%s-%s", hhmm(c.WindowStart), hhmm(c.WindowEnd))
}

func hhmm(minutes int) string {
	return fmt.Sprintf("%02d:%02d", minutes/60, minutes%60)
}

// Load 从 getenv 读取并校验全部配置。getenv 形如 os.Getenv 的变体
// （如 strings.OS 环境下由 environ 切片构建的 LookupEnv），便于测试注入。
// 返回的 error 在多个违规项并存时包含全部问题（每一行一项），退出码由调用方决定。
func Load(getenv func(string) (string, bool)) (*Config, error) {
	cfg := &Config{
		DataDir: DefaultDataDir,
		TZName:  DefaultTZName,
	}
	var problems []string

	// Cookie：可选（无持久化 Login Session 时由 app 层快速失败）。
	if raw, ok := getenv(EnvCookie); ok {
		cfg.Cookie = strings.TrimSpace(raw)
		if cfg.Cookie == "" {
			problems = append(problems, EnvCookie+" 只含空白字符")
		}
	}

	// Books：逗号分隔 bookId，可选。整体为空 = 未指定（自动从 Shelf 选书，User Story 19）；
	// 列表内部出现空元素视为书写错误（如 "a,,b"），明确报错。
	if raw, ok := getenv(EnvBooks); ok && strings.TrimSpace(raw) != "" {
		for i, part := range strings.Split(raw, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				problems = append(problems, fmt.Sprintf("%s 第 %d 项为空（bookId 不能为空）", EnvBooks, i+1))
				continue
			}
			cfg.Books = append(cfg.Books, part)
		}
	}

	// Run Window：HH:MM，必需。
	var okStart, okEnd bool
	cfg.WindowStart, okStart = parseWindow(EnvWindowStart, getenv, &problems)
	cfg.WindowEnd, okEnd = parseWindow(EnvWindowEnd, getenv, &problems)
	if okStart && okEnd && cfg.WindowStart != cfg.WindowEnd && cfg.WindowStart > cfg.WindowEnd {
		problems = append(problems, fmt.Sprintf(
			"运行窗口 %s (%s) 晚于 %s (%s)；start==end 合法（固定启动时刻），start>end 非法",
			EnvWindowStart, hhmm(cfg.WindowStart), EnvWindowEnd, hhmm(cfg.WindowEnd)))
	}

	// Target Duration 范围（分钟），必需。
	var okMin, okMax bool
	cfg.ReadMinutesMin, okMin = parseMinutes(EnvReadMin, getenv, &problems)
	cfg.ReadMinutesMax, okMax = parseMinutes(EnvReadMax, getenv, &problems)
	if okMin && okMax && cfg.ReadMinutesMin > cfg.ReadMinutesMax {
		problems = append(problems, fmt.Sprintf("%s (%d) 大于 %s (%d)", EnvReadMin, cfg.ReadMinutesMin, EnvReadMax, cfg.ReadMinutesMax))
	}

	// 通知渠道（可选），非空时必须是合法 http(s) URL。
	if raw, ok := getenv(EnvBarkURL); ok && strings.TrimSpace(raw) != "" {
		if v, okURL := parseHTTPURL(raw); okURL {
			cfg.BarkURL = v
		} else {
			problems = append(problems, fmt.Sprintf("%s: 不是有效的 http(s) URL: %q", EnvBarkURL, strings.TrimSpace(raw)))
		}
	}
	if raw, ok := getenv(EnvWeComURL); ok && strings.TrimSpace(raw) != "" {
		if v, okURL := parseHTTPURL(raw); okURL {
			cfg.WeComWebhookURL = v
		} else {
			problems = append(problems, fmt.Sprintf("%s: 不是有效的 http(s) URL: %q", EnvWeComURL, strings.TrimSpace(raw)))
		}
	}

	// DataDir，默认 /data。
	if raw, ok := getenv(EnvDataDir); ok {
		cfg.DataDir = strings.TrimSpace(raw)
		if cfg.DataDir == "" {
			problems = append(problems, EnvDataDir+" 只含空白字符")
		}
	}

	// TZ，默认 Asia/Shanghai；显式空值视为未设置（保持默认）。
	if raw, ok := getenv(EnvTZ); ok && strings.TrimSpace(raw) != "" {
		cfg.TZName = strings.TrimSpace(raw)
	}
	loc, err := time.LoadLocation(cfg.TZName)
	if err != nil {
		problems = append(problems, fmt.Sprintf("%s: 无法识别的时区 %q（二进制内嵌 tzdata）", EnvTZ, cfg.TZName))
	} else {
		cfg.TZ = loc
	}

	if len(problems) > 0 {
		return nil, &ValidationError{Problems: problems}
	}
	return cfg, nil
}

// ValidationError 汇总全部配置违规项；Error() 输出多行清单，每行一个具体违规项。
type ValidationError struct {
	Problems []string
}

func (e *ValidationError) Error() string {
	var b strings.Builder
	b.WriteString("配置无效:\n")
	for _, p := range e.Problems {
		b.WriteString("  - " + p + "\n")
	}
	return b.String()
}

// parseWindow 解析 HH:MM，返回自当天 00:00 起的分钟数；解析失败时把具体违规项加入 problems。
func parseWindow(env string, getenv func(string) (string, bool), problems *[]string) (int, bool) {
	raw, ok := getenv(env)
	if !ok {
		*problems = append(*problems, env+" 未设置（必需）")
		return 0, false
	}
	raw = strings.TrimSpace(raw)
	t, err := time.Parse(windowTimeLayout, raw)
	if err != nil {
		*problems = append(*problems, fmt.Sprintf("%s: 时间格式非法（要求 HH:MM，24 小时制）: %q", env, raw))
		return 0, false
	}
	return t.Hour()*60 + t.Minute(), true
}

// parseMinutes 解析正整数分钟数；解析失败时把具体违规项加入 problems。
func parseMinutes(env string, getenv func(string) (string, bool), problems *[]string) (int, bool) {
	raw, ok := getenv(env)
	if !ok {
		*problems = append(*problems, env+" 未设置（必需）")
		return 0, false
	}
	raw = strings.TrimSpace(raw)
	n, err := strconv.Atoi(raw)
	if err != nil {
		*problems = append(*problems, fmt.Sprintf("%s: 不是有效整数: %q", env, raw))
		return 0, false
	}
	if n < 1 {
		*problems = append(*problems, fmt.Sprintf("%s: 必须大于 0（got %d）", env, n))
		return 0, false
	}
	return n, true
}

func parseHTTPURL(raw string) (string, bool) {
	v := strings.TrimSpace(raw)
	u, err := url.Parse(v)
	if err != nil {
		return "", false
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", false
	}
	return v, true
}
