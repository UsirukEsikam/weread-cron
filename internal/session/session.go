// Package session 管理 Login Session（微信读书登录凭据）的 Cookie 集合：
// 从初始 Cookie header 建立、renewal 返回的 Set-Cookie 并入、序列化/恢复与原子持久化。
//
// tick 03 范围：初始 Cookie 建立 Login Session、renewal 新 Cookie 并入会话并落盘。
// 失效判别与从初始 Cookie 重建（ticket 04）不在本包。
package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"weread-cron/internal/atomicfile"
)

// 持久化文件名（ticket 04 沿用同一格式扩展字段）。
const (
	loginSessionFileName = "login_session.json"
)

// Store 抽象 Login Session 的持久化存储（ADR-0006：经构造器注入）。
type Store interface {
	// HasLoginSession 报告是否存在可恢复的持久化 Login Session。
	HasLoginSession() (bool, error)
	// LoadJar 读取完整 Cookie 集合（属性完整保存）。
	LoadJar() (*Jar, error)
	// SaveJar 原子持久化（临时文件 + rename，写入中途崩溃不留半写状态）。
	SaveJar(j *Jar) error
}

// CookieHeader 解析/构造所需的客户端函数（见 weread.Client）。
// Session 持有 Jar 与 Store，向 HTTP 层提供 Cookie 读取与 Set-Cookie 合并。

// Jar 是 Login Session 的 Cookie 集合，按名字索引，属性（Domain/Path/Expires 等）完整保存。
type Jar struct {
	cookies map[string]*http.Cookie
}

// NewJar 创建空 Jar。
func NewJar() *Jar {
	return &Jar{cookies: map[string]*http.Cookie{}}
}

// NewJarFromHeader 从形如 "a=1; b=2" 的 Cookie header 字符串建立 Cookie 集合。
// 只保留有名字的项；值可为空串。
func NewJarFromHeader(header string) (*Jar, error) {
	j := NewJar()
	parts := strings.Split(header, ";")
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		i := strings.IndexByte(part, '=')
		var name, value string
		if i < 0 {
			name, value = part, ""
		} else {
			name, value = part[:i], part[i+1:]
		}
		name = strings.TrimSpace(name)
		if name == "" {
			return nil, fmt.Errorf("Cookie header 含空名字的条目: %q", header)
		}
		j.cookies[name] = &http.Cookie{Name: name, Value: value}
	}
	return j, nil
}

// CookieHeader 输出按名字排序的 "k=v; k2=v2" 串（排序保证确定性与可断言性）。
func (j *Jar) CookieHeader() string {
	names := make([]string, 0, len(j.cookies))
	for name := range j.cookies {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, name+"="+j.cookies[name].Value)
	}
	return strings.Join(parts, "; ")
}

// MergeSetCookies 把 Set-Cookie 属性并入集合（同名替换，属性随新值更新）。
func (j *Jar) MergeSetCookies(cookies []*http.Cookie) {
	for _, c := range cookies {
		if c == nil || c.Name == "" {
			continue
		}
		j.cookies[c.Name] = c
	}
}

// Snapshot 返回按名字排序的 Cookie 副本（持久化用）。
func (j *Jar) Snapshot() []*http.Cookie {
	names := make([]string, 0, len(j.cookies))
	for name := range j.cookies {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]*http.Cookie, 0, len(names))
	for _, name := range names {
		c := *j.cookies[name]
		out = append(out, &c)
	}
	return out
}

// cookieRecord 是 Cookie 属性完整保存的持久化条目（spec 决策 #9）。
type cookieRecord struct {
	Name     string    `json:"name"`
	Value    string    `json:"value"`
	Path     string    `json:"path,omitempty"`
	Domain   string    `json:"domain,omitempty"`
	Expires  time.Time `json:"expires,omitempty"`
	MaxAge   int       `json:"max_age,omitempty"`
	Secure   bool      `json:"secure,omitempty"`
	HTTPOnly bool      `json:"http_only,omitempty"`
	SameSite string    `json:"same_site,omitempty"`
	Raw      string    `json:"raw,omitempty"`
}

type sessionFile struct {
	Cookies []cookieRecord `json:"cookies"`
}

func encodeJar(j *Jar) ([]byte, error) {
	records := make([]cookieRecord, 0, len(j.cookies))
	for _, c := range j.Snapshot() {
		records = append(records, cookieRecord{
			Name:     c.Name,
			Value:    c.Value,
			Path:     c.Path,
			Domain:   c.Domain,
			Expires:  c.Expires,
			MaxAge:   c.MaxAge,
			Secure:   c.Secure,
			HTTPOnly: c.HttpOnly,
			SameSite: sameSiteString(c.SameSite),
			Raw:      c.Raw,
		})
	}
	return json.MarshalIndent(sessionFile{Cookies: records}, "", "  ")
}

func decodeJar(data []byte) (*Jar, error) {
	var f sessionFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("解析 Login Session 文件失败: %w", err)
	}
	j := NewJar()
	for _, r := range f.Cookies {
		if r.Name == "" {
			continue
		}
		j.cookies[r.Name] = &http.Cookie{
			Name:     r.Name,
			Value:    r.Value,
			Path:     r.Path,
			Domain:   r.Domain,
			Expires:  r.Expires,
			MaxAge:   r.MaxAge,
			Secure:   r.Secure,
			HttpOnly: r.HTTPOnly,
			SameSite: parseSameSite(r.SameSite),
			Raw:      r.Raw,
		}
	}
	return j, nil
}

func sameSiteString(s http.SameSite) string {
	switch s {
	case http.SameSiteDefaultMode:
		return "default"
	case http.SameSiteLaxMode:
		return "lax"
	case http.SameSiteStrictMode:
		return "strict"
	case http.SameSiteNoneMode:
		return "none"
	}
	return ""
}

func parseSameSite(s string) http.SameSite {
	switch strings.ToLower(s) {
	case "lax":
		return http.SameSiteLaxMode
	case "strict":
		return http.SameSiteStrictMode
	case "none":
		return http.SameSiteNoneMode
	}
	return http.SameSiteDefaultMode
}

// Session 是Task/应用层使用的 Login Session 句柄：读取 Cookie header、并入并保存新 Cookie。
type Session struct {
	jar   *Jar
	store Store
}

// New 建立/恢复 Login Session（spec 决策 #2、用户故事 #7/#8）：
//   - 存在持久化会话 → 恢复（ticket 04 验证"恢复优先于初始 Cookie"的细节语义）；
//   - 否则用初始 Cookie 建立并立即持久化；
//   - 两者皆无 → 错误（CLI 层已前置校验，此错误仅防御）。
func New(store Store, initialCookie string) (*Session, error) {
	has, err := store.HasLoginSession()
	if err != nil {
		return nil, fmt.Errorf("检查 Login Session 失败: %w", err)
	}
	if has {
		jar, err := store.LoadJar()
		if err != nil {
			return nil, fmt.Errorf("恢复 Login Session 失败: %w", err)
		}
		if len(jar.cookies) == 0 {
			return nil, errors.New("持久化 Login Session 为空，请重新提供初始 Cookie")
		}
		return &Session{jar: jar, store: store}, nil
	}
	if strings.TrimSpace(initialCookie) == "" {
		return nil, errors.New("无持久化 Login Session 且未配置初始 Cookie")
	}
	jar, err := NewJarFromHeader(initialCookie)
	if err != nil {
		return nil, fmt.Errorf("解析初始 Cookie 失败: %w", err)
	}
	if err := store.SaveJar(jar); err != nil {
		return nil, fmt.Errorf("持久化初始 Login Session 失败: %w", err)
	}
	return &Session{jar: jar, store: store}, nil
}

// CookieHeader 返回发送给微信读书的 Cookie header。
func (s *Session) CookieHeader() string { return s.jar.CookieHeader() }

// MergeAndSaveCookies 把响应 Set-Cookie 并入会话并原子持久化（renewal 后重启不回退旧会话）。
func (s *Session) MergeAndSaveCookies(cookies []*http.Cookie) error {
	if len(cookies) == 0 {
		return nil
	}
	s.jar.MergeSetCookies(cookies)
	if err := s.store.SaveJar(s.jar); err != nil {
		return fmt.Errorf("持久化更新后的 Login Session 失败: %w", err)
	}
	return nil
}

// FileStore 将 Login Session 存储在 DataDir 下。
type FileStore struct {
	dir string
}

// NewFileStore 创建基于目录 dir 的文件存储。
func NewFileStore(dir string) *FileStore {
	return &FileStore{dir: dir}
}

func (s *FileStore) path() string { return filepath.Join(s.dir, loginSessionFileName) }

// HasLoginSession 检查 DataDir 下是否存在 Login Session 文件。
func (s *FileStore) HasLoginSession() (bool, error) {
	_, err := os.Stat(s.path())
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// LoadJar 读取并解析 Login Session 文件。
func (s *FileStore) LoadJar() (*Jar, error) {
	data, err := os.ReadFile(s.path())
	if err != nil {
		return nil, err
	}
	return decodeJar(data)
}

// SaveJar 原子写入 Login Session 文件。
func (s *FileStore) SaveJar(j *Jar) error {
	data, err := encodeJar(j)
	if err != nil {
		return err
	}
	return atomicfile.WriteFile(s.path(), data)
}
