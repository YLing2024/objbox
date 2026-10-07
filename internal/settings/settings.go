// Package settings 负责 objbox 服务端级设置的持久化（当前仅跨域白名单）。
//
// 设置文件 settings.json 与 accounts.json 同目录，权限 0600；写入一律
// 「临时文件 → fsync → rename」原子替换。文件损坏（JSON 非法或含非法来源）时
// 保留原文件为 settings.json.bak，并临时回退到环境变量默认值，不影响服务启动。
//
// 生效优先级：设置文件存在 corsOrigins 键 > 环境变量 CORS_ORIGINS（逗号分隔）
// > 都没有 = 关闭跨域。保存设置时同步更新内存，无需重启即生效。
package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

const (
	// FileName 是设置文件名，位于数据目录下。
	FileName = "settings.json"
	// FileMode 是设置文件权限。
	FileMode = 0o600
	// Version 是当前设置文件格式版本。
	Version = 1
	// EnvCORSOrigins 是跨域白名单的环境变量名（逗号分隔），仅作首次默认值。
	EnvCORSOrigins = "CORS_ORIGINS"
)

// Source 描述当前生效的跨域白名单来自哪里，供管理页展示。
type Source string

const (
	// SourceNone 表示未开启跨域。
	SourceNone Source = "none"
	// SourceEnv 表示当前值来自环境变量 CORS_ORIGINS（设置文件尚无该键）。
	SourceEnv Source = "env"
	// SourceSettings 表示当前值来自设置文件。
	SourceSettings Source = "settings"
)

// fileFormat 是 settings.json 的磁盘结构。
//
// CORSOrigins 用指针区分「键缺失」与「显式空列表」：键缺失时回退环境变量，
// 显式空列表表示用户主动关闭跨域。
type fileFormat struct {
	Version     int       `json:"version"`
	CORSOrigins *[]string `json:"corsOrigins"`
}

// Store 是加载到内存的设置表。
type Store struct {
	path    string
	dataDir string

	mu sync.RWMutex
	// fileOr 为设置文件里的白名单；nil 表示文件没有该键。
	fileOr *[]string
	// envOr 为环境变量解析出的初始默认值。
	envOr []string
}

// Load 从 dataDir/settings.json 读取设置；文件不存在返回空设置。
//
// 文件损坏时把原文件保留为 .bak 并使用环境变量默认值，不返回错误。
func Load(dataDir string) (*Store, error) {
	if dataDir == "" {
		return nil, errors.New("settings: 数据目录不能为空")
	}
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, fmt.Errorf("settings: 解析数据目录失败: %w", err)
	}
	s := &Store{
		path:    filepath.Join(abs, FileName),
		dataDir: abs,
		envOr:   parseEnv(os.Getenv(EnvCORSOrigins)),
	}
	s.loadFile()
	return s, nil
}

// Path 返回设置文件路径。
func (s *Store) Path() string { return s.path }

// DataDir 返回数据目录。
func (s *Store) DataDir() string { return s.dataDir }

// loadFile 读取并解析设置文件。失败时保留旧值（此处为环境变量默认值）并把
// 原文件重命名为 .bak。
func (s *Store) loadFile() {
	raw, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		log.Printf("settings: 读取 %s 失败，使用默认设置: %v", s.path, err)
		return
	}
	var f fileFormat
	if err := json.Unmarshal(raw, &f); err != nil {
		s.quarantine(raw, "JSON 解析失败: "+err.Error())
		return
	}
	if f.CORSOrigins == nil {
		return
	}
	normalized, err := NormalizeAll(*f.CORSOrigins)
	if err != nil {
		s.quarantine(raw, err.Error())
		return
	}
	s.fileOr = &normalized
}

// quarantine 把损坏的设置文件保留为 .bak，并记录日志。原文件无法重命名时
// 退化为把原始内容写入 .bak。
func (s *Store) quarantine(raw []byte, why string) {
	bak := s.path + ".bak"
	if err := os.Rename(s.path, bak); err != nil {
		log.Printf("settings: %s 损坏（%s），重命名为 %s 失败: %v", s.path, why, bak, err)
		if werr := os.WriteFile(bak, raw, FileMode); werr != nil {
			log.Printf("settings: 写入 %s 失败: %v", bak, werr)
		}
	} else {
		log.Printf("settings: %s 损坏（%s），已保留为 %s，使用默认设置", s.path, why, bak)
	}
}

// Origins 返回当前生效的跨域白名单副本；空表示关闭跨域。
func (s *Store) Origins() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]string(nil), s.effectiveLocked()...)
}

// Allows 判断来源是否命中白名单。
func (s *Store) Allows(origin string) bool {
	if origin == "" {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, o := range s.effectiveLocked() {
		if o == origin {
			return true
		}
	}
	return false
}

// Source 返回当前生效值来源。
func (s *Store) Source() Source {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.fileOr != nil {
		return SourceSettings
	}
	if len(s.envOr) > 0 {
		return SourceEnv
	}
	return SourceNone
}

// effectiveLocked 返回当前生效白名单（需持锁）。
func (s *Store) effectiveLocked() []string {
	if s.fileOr != nil {
		return *s.fileOr
	}
	return s.envOr
}

// SetCORSOrigins 校验并规范化后原子写入设置文件，并同步更新内存（保存即生效）。
// 校验失败返回带「第 N 行」定位的错误，且不修改任何状态。
func (s *Store) SetCORSOrigins(origins []string) error {
	normalized, err := NormalizeAll(origins)
	if err != nil {
		return err
	}
	f := fileFormat{Version: Version, CORSOrigins: &normalized}
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("settings: 序列化设置失败: %w", err)
	}
	raw = append(raw, '\n')
	if err := writeFileAtomic(s.path, raw); err != nil {
		return err
	}
	s.mu.Lock()
	s.fileOr = &normalized
	s.mu.Unlock()
	return nil
}

// NormalizeAll 逐个规范化并校验来源；错误信息带「第 N 行」定位，不静默丢弃。
func NormalizeAll(origins []string) ([]string, error) {
	out := make([]string, 0, len(origins))
	for i, raw := range origins {
		norm, err := Normalize(raw)
		if err != nil {
			return nil, fmt.Errorf("第 %d 行来源非法：%s（%s）", i+1, strings.TrimSpace(raw), err)
		}
		out = append(out, norm)
	}
	return out, nil
}

// Normalize 校验并规范化单个来源：必须是 scheme://host 或 scheme://host:port，
// scheme 限 http/https，不得带路径 / 查询 / 片段 / 用户名密码。允许一个末尾斜杠。
func Normalize(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", errors.New("来源不能为空")
	}
	trimmed := strings.TrimSuffix(s, "/")
	u, err := url.Parse(trimmed)
	if err != nil {
		return "", errors.New("不是合法的 URL")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", errors.New("只支持 http 或 https")
	}
	if u.User != nil {
		return "", errors.New("不得包含用户名或密码")
	}
	if u.Hostname() == "" {
		return "", errors.New("缺少主机名")
	}
	if u.Path != "" {
		return "", errors.New("只能填 scheme://host[:port]，不得带路径")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("不得带查询或片段")
	}
	if port := u.Port(); port != "" {
		n, perr := strconv.Atoi(port)
		if perr != nil || n < 1 || n > 65535 {
			return "", errors.New("端口号非法")
		}
	}
	return scheme + "://" + u.Host, nil
}

// parseEnv 解析逗号分隔的环境变量；非法项记录日志后跳过，不影响启动。
func parseEnv(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		norm, err := Normalize(p)
		if err != nil {
			log.Printf("settings: 忽略环境变量 %s 中非法来源 %q: %v", EnvCORSOrigins, p, err)
			continue
		}
		out = append(out, norm)
	}
	return out
}

// writeFileAtomic 以 FileMode 权限原子写入文件（临时文件 + fsync + rename）。
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("settings: 创建数据目录失败: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".settings-*.tmp")
	if err != nil {
		return fmt.Errorf("settings: 创建临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if err := tmp.Chmod(FileMode); err != nil {
		tmp.Close()
		return fmt.Errorf("settings: 设置权限失败: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("settings: 写入设置失败: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("settings: fsync 设置失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("settings: 关闭临时文件失败: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("settings: 替换 %s 失败: %w", path, err)
	}
	return nil
}
