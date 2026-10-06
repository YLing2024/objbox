// Package account 负责账号模型、accounts.json 的读写、AK→账号索引与 AK/SK 生成。
//
// 设计取舍：S3 的 SigV4 校验要求服务端持有明文 SK 才能重算 HMAC 链，
// 因此 SK 无法只存哈希，只能明文落盘，靠文件权限 0600 保护。
package account

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/YLing2024/objbox/internal/randstr"
)

const (
	// FileName 是账号表文件名，位于数据目录下。
	FileName = "accounts.json"
	// FileMode 是账号表文件权限，含明文 SK，必须为 0600。
	FileMode = 0o600

	// Version 是当前账号表格式版本。
	Version = 1

	// AKPrefix 是生成的 AK 前缀，便于识别与检索。
	AKPrefix = "AK"
	// akRandomBytes 为 AK 随机部分字节数，base32 后 32 字符，
	// 加上前缀总长 34，落在需求要求的 32～40 区间内。
	akRandomBytes = 20
	// skRandomBytes 为 SK 随机部分字节数，base64url 后 43 字符。
	skRandomBytes = 32
)

// nameRe 限制账号名，避免路径分隔符等危险字符。
var nameRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// Account 是一个账号。一个账号对应一套 AK/SK 与一个隔离根目录。
type Account struct {
	Name       string `json:"name"`
	AK         string `json:"ak"`
	SK         string `json:"sk"`
	Root       string `json:"root"`
	Readonly   bool   `json:"readonly"`
	Disabled   bool   `json:"disabled"`
	QuotaBytes int64  `json:"quotaBytes"`
	Note       string `json:"note"`
}

// Clone 返回账号的深拷贝，避免外部修改内部状态。
func (a *Account) Clone() *Account {
	if a == nil {
		return nil
	}
	c := *a
	return &c
}

// fileFormat 是 accounts.json 的磁盘结构。
type fileFormat struct {
	Version  int        `json:"version"`
	Accounts []*Account `json:"accounts"`
}

// Store 是加载到内存的账号表，并在 AK→账号上建立索引。
type Store struct {
	path    string
	dataDir string

	mu       sync.RWMutex
	accounts []*Account
	byAK     map[string]*Account
}

// Load 从 dataDir/accounts.json 读取账号表。文件不存在时返回空表（不报错）。
//
// root 为空时按 dataDir/roots/<name> 推导；加载时校验 name 与 AK 的唯一性。
func Load(dataDir string) (*Store, error) {
	if dataDir == "" {
		return nil, errors.New("account: 数据目录不能为空")
	}
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, fmt.Errorf("account: 解析数据目录失败: %w", err)
	}
	s := &Store{
		path:    filepath.Join(abs, FileName),
		dataDir: abs,
		byAK:    map[string]*Account{},
	}

	raw, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("account: 读取 %s 失败: %w", s.path, err)
	}

	var f fileFormat
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("account: 解析 %s 失败: %w", s.path, err)
	}

	seenName := map[string]bool{}
	seenAK := map[string]bool{}
	for _, a := range f.Accounts {
		if a == nil {
			continue
		}
		if !nameRe.MatchString(a.Name) {
			return nil, fmt.Errorf("account: 非法账号名 %q", a.Name)
		}
		if seenName[a.Name] {
			return nil, fmt.Errorf("account: 账号名重复 %q", a.Name)
		}
		seenName[a.Name] = true

		if a.AK == "" || a.SK == "" {
			return nil, fmt.Errorf("account: 账号 %q 缺少 AK 或 SK", a.Name)
		}
		if seenAK[a.AK] {
			return nil, fmt.Errorf("account: AK 重复 %q", a.AK)
		}
		seenAK[a.AK] = true

		if a.Root == "" {
			a.Root = filepath.Join(abs, "roots", a.Name)
		}
		a.Root, err = filepath.Abs(a.Root)
		if err != nil {
			return nil, fmt.Errorf("account: 解析账号 %q 的 root 失败: %w", a.Name, err)
		}

		s.accounts = append(s.accounts, a)
		s.byAK[a.AK] = a
	}
	return s, nil
}

// Path 返回账号表文件路径。
func (s *Store) Path() string { return s.path }

// DataDir 返回数据目录。
func (s *Store) DataDir() string { return s.dataDir }

// List 返回账号副本列表（按加载顺序）。
func (s *Store) List() []*Account {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Account, 0, len(s.accounts))
	for _, a := range s.accounts {
		out = append(out, a.Clone())
	}
	return out
}

// GetByAK 按 AK 查账号。返回的是内部指针，调用方只读。
func (s *Store) GetByAK(ak string) (*Account, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.byAK[ak]
	return a, ok
}

// Find 按名字查账号。返回的是内部指针，调用方只读。
func (s *Store) Find(name string) (*Account, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, a := range s.accounts {
		if a.Name == name {
			return a, true
		}
	}
	return nil, false
}

// Add 新建账号并落盘。返回的 SK 仅在此刻可见，调用方应只打印一次。
func (s *Store) Add(name, note string, readonly bool) (*Account, error) {
	if !nameRe.MatchString(name) {
		return nil, fmt.Errorf("account: 非法账号名 %q（只允许字母数字 . _ -，长度 1-64）", name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.findLocked(name); ok {
		return nil, fmt.Errorf("account: 账号 %q 已存在", name)
	}

	ak, err := s.genUniqueAKLocked()
	if err != nil {
		return nil, err
	}
	sk, err := randstr.Token(skRandomBytes)
	if err != nil {
		return nil, fmt.Errorf("account: 生成 SK 失败: %w", err)
	}

	a := &Account{
		Name:     name,
		AK:       ak,
		SK:       sk,
		Root:     filepath.Join(s.dataDir, "roots", name),
		Readonly: readonly,
		Note:     note,
	}
	if err := os.MkdirAll(a.Root, 0o700); err != nil {
		return nil, fmt.Errorf("account: 创建 root 目录失败: %w", err)
	}

	s.accounts = append(s.accounts, a)
	s.byAK[a.AK] = a
	if err := s.saveLocked(); err != nil {
		// 回滚内存状态，保持与磁盘一致。
		s.accounts = s.accounts[:len(s.accounts)-1]
		delete(s.byAK, a.AK)
		return nil, err
	}
	return a.Clone(), nil
}

// Rotate 轮换账号 SK，返回新 SK（仅此一次可见）。旧 SK 立即失效。
func (s *Store) Rotate(name string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.findLocked(name)
	if !ok {
		return "", fmt.Errorf("account: 账号 %q 不存在", name)
	}
	sk, err := randstr.Token(skRandomBytes)
	if err != nil {
		return "", fmt.Errorf("account: 生成 SK 失败: %w", err)
	}
	old := a.SK
	a.SK = sk
	if err := s.saveLocked(); err != nil {
		a.SK = old
		return "", err
	}
	return sk, nil
}

// SetDisabled 启用/停用账号。
func (s *Store) SetDisabled(name string, disabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.findLocked(name)
	if !ok {
		return fmt.Errorf("account: 账号 %q 不存在", name)
	}
	old := a.Disabled
	a.Disabled = disabled
	if err := s.saveLocked(); err != nil {
		a.Disabled = old
		return err
	}
	return nil
}

// Remove 从账号表移除账号（不删除其 root 数据目录，避免误删）。
func (s *Store) Remove(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := -1
	for i, a := range s.accounts {
		if a.Name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("account: 账号 %q 不存在", name)
	}
	removed := s.accounts[idx]
	s.accounts = append(s.accounts[:idx], s.accounts[idx+1:]...)
	delete(s.byAK, removed.AK)
	if err := s.saveLocked(); err != nil {
		s.accounts = append(s.accounts, nil)
		copy(s.accounts[idx+1:], s.accounts[idx:])
		s.accounts[idx] = removed
		s.byAK[removed.AK] = removed
		return err
	}
	return nil
}

func (s *Store) findLocked(name string) (*Account, bool) {
	for _, a := range s.accounts {
		if a.Name == name {
			return a, true
		}
	}
	return nil, false
}

func (s *Store) genUniqueAKLocked() (string, error) {
	for i := 0; i < 16; i++ {
		body, err := randstr.Base32Upper(akRandomBytes)
		if err != nil {
			return "", fmt.Errorf("account: 生成 AK 失败: %w", err)
		}
		ak := AKPrefix + body
		if _, exists := s.byAK[ak]; !exists {
			return ak, nil
		}
	}
	return "", errors.New("account: 生成唯一 AK 失败（多次碰撞）")
}

// saveLocked 以 0600 权限原子写回账号表。调用方必须已持有写锁。
func (s *Store) saveLocked() error {
	f := fileFormat{Version: Version, Accounts: s.accounts}
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("account: 序列化账号表失败: %w", err)
	}
	raw = append(raw, '\n')

	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("account: 创建数据目录失败: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".accounts-*.tmp")
	if err != nil {
		return fmt.Errorf("account: 创建临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if err := tmp.Chmod(FileMode); err != nil {
		tmp.Close()
		return fmt.Errorf("account: 设置权限失败: %w", err)
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return fmt.Errorf("account: 写入账号表失败: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("account: fsync 账号表失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("account: 关闭临时文件失败: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("account: 替换账号表失败: %w", err)
	}
	return nil
}

// MaskSecret 返回 SK 的掩码形式（前 4 后 4），用于列表展示。
func MaskSecret(sk string) string {
	if len(sk) <= 8 {
		return strings.Repeat("*", len(sk))
	}
	return sk[:4] + "****" + sk[len(sk)-4:]
}

// ModifiedAt 仅用于展示，返回账号表最后修改时间（不存在则零值）。
func (s *Store) ModifiedAt() time.Time {
	fi, err := os.Stat(s.path)
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}
