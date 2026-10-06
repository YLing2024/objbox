// Package account 负责账号模型、accounts.json 的读写、AK→账号索引与 AK/SK 生成。
//
// 设计取舍：S3 的 SigV4 校验要求服务端持有明文 SK 才能重算 HMAC 链，
// 因此 SK 无法只存哈希，只能明文落盘，靠文件权限 0600 保护。
//
// 热重载：服务进程会周期性地检查 accounts.json 的 mtime+size，
// 变化时重新解析并原子替换内存账号表；解析失败保留旧表，绝不清空。
// 已发布的 *Account 在替换后不再被修改（写路径采用 copy-on-write），
// 因此读路径上的指针即使越过读锁也是安全的，go test -race 无竞争。
package account

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
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

	// DefaultReloadInterval 是热重载检查的默认节流间隔。
	// 处理请求前最多每 1 秒 stat 一次 accounts.json。
	DefaultReloadInterval = time.Second
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

// fileSig 是判断账号表是否变化的轻量判据（大小 + 修改时间）。
type fileSig struct {
	size int64
	mod  time.Time
}

// Store 是加载到内存的账号表，并在 AK→账号上建立索引。
//
// accounts / byAK / sig 受 mu 保护；已发布的 *Account 视为不可变，
// 因此 GetByAK 返回的指针在释放读锁后仍可安全读取。
type Store struct {
	path    string
	dataDir string

	mu       sync.RWMutex
	accounts []*Account
	byAK     map[string]*Account

	// sig 记录上次加载/保存时账号表文件的大小与 mtime。
	sig     fileSig
	haveSig bool

	// reloadMu 保证同一时刻只有一次热重载在跑。
	reloadMu sync.Mutex
	// intervalNanos 为检查节流间隔（纳秒），0 表示每次请求都检查。
	intervalNanos atomic.Int64
	// lastCheck 记录上次检查时间（Unix 纳秒），0 表示尚未检查。
	lastCheck atomic.Int64
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
	s.intervalNanos.Store(int64(DefaultReloadInterval))

	raw, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("account: 读取 %s 失败: %w", s.path, err)
	}

	accounts, err := parseAccounts(raw, abs, s.path)
	if err != nil {
		return nil, err
	}
	s.accounts = accounts
	s.byAK = indexByAK(accounts)
	s.refreshSigLocked()
	return s, nil
}

// parseAccounts 解析并校验账号表，返回新的（不可变）账号切片。
func parseAccounts(raw []byte, dataDir, path string) ([]*Account, error) {
	var f fileFormat
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("account: 解析 %s 失败: %w", path, err)
	}

	seenName := map[string]bool{}
	seenAK := map[string]bool{}
	var accounts []*Account
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
			a.Root = filepath.Join(dataDir, "roots", a.Name)
		}
		root, err := filepath.Abs(a.Root)
		if err != nil {
			return nil, fmt.Errorf("account: 解析账号 %q 的 root 失败: %w", a.Name, err)
		}
		a.Root = root

		accounts = append(accounts, a)
	}
	return accounts, nil
}

func indexByAK(accounts []*Account) map[string]*Account {
	m := make(map[string]*Account, len(accounts))
	for _, a := range accounts {
		m[a.AK] = a
	}
	return m
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

// Count 返回当前账号数。
func (s *Store) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.accounts)
}

// GetByAK 按 AK 查账号。返回内部不可变指针，调用方只读。
func (s *Store) GetByAK(ak string) (*Account, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.byAK[ak]
	return a, ok
}

// Find 按名字查账号。返回内部不可变指针，调用方只读。
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

// SetReloadInterval 设置热重载的检查节流间隔；<=0 表示每次检查都 stat。
// 应在服务开始处理请求前调用。
func (s *Store) SetReloadInterval(d time.Duration) {
	if d < 0 {
		d = 0
	}
	s.intervalNanos.Store(int64(d))
}

// MaybeReload 检查 accounts.json 是否变化（mtime+size），变化则重新加载并
// 原子替换内存账号表。解析失败或文件读取失败时保留旧表并记录一条日志。
//
// 处理请求前调用；受 intervalNanos 节流，默认最多每秒检查一次。
func (s *Store) MaybeReload() {
	if d := s.intervalNanos.Load(); d > 0 {
		if last := s.lastCheck.Load(); last != 0 &&
			time.Since(time.Unix(0, last)) < time.Duration(d) {
			return
		}
	}
	if !s.reloadMu.TryLock() {
		return
	}
	defer s.reloadMu.Unlock()

	if d := s.intervalNanos.Load(); d > 0 {
		if last := s.lastCheck.Load(); last != 0 &&
			time.Since(time.Unix(0, last)) < time.Duration(d) {
			return
		}
	}
	s.lastCheck.Store(time.Now().UnixNano())

	changed, err := s.reloadLocked(false)
	if err != nil {
		log.Printf("account: accounts.json 热重载失败，继续使用旧账号表: %v", err)
		return
	}
	if changed {
		log.Printf("account: accounts.json 已热重载，当前账号数 %d", s.Count())
	}
}

// Reload 强制从磁盘重新加载账号表（忽略 mtime 判据，仍受 reloadMu 串行化）。
// 解析失败时保留旧表并返回错误，不会清空内存账号表。
func (s *Store) Reload() error {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	_, err := s.reloadLocked(true)
	return err
}

// reloadLocked 读取并解析账号表，成功后原子替换内存表。
//
// force 为 true 时忽略 mtime+size 判据直接解析（仍先完成校验再替换）；
// 为 false 时若文件签名未变化则直接返回 changed=false。
// 任何失败都不会修改现有内存表。调用方需持有 reloadMu。
func (s *Store) reloadLocked(force bool) (changed bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	fi, statErr := os.Stat(s.path)
	if errors.Is(statErr, os.ErrNotExist) {
		// 文件被删除：保留旧表，避免误伤在线账号。
		return false, nil
	}
	if statErr != nil {
		return false, fmt.Errorf("account: stat %s 失败: %w", s.path, statErr)
	}
	sig := fileSig{size: fi.Size(), mod: fi.ModTime()}
	if !force && s.haveSig && sig == s.sig {
		return false, nil
	}

	raw, err := os.ReadFile(s.path)
	if err != nil {
		return false, fmt.Errorf("account: 读取 %s 失败: %w", s.path, err)
	}
	accounts, err := parseAccounts(raw, s.dataDir, s.path)
	if err != nil {
		return false, err
	}

	// 校验全部通过后才替换，保证原子性。
	s.accounts = accounts
	s.byAK = indexByAK(accounts)
	s.sig = sig
	s.haveSig = true
	return true, nil
}

// refreshSigLocked 在写盘成功后刷新文件签名，避免本进程的写入被误判为外部变更。
// 调用方需持有写锁。
func (s *Store) refreshSigLocked() {
	fi, err := os.Stat(s.path)
	if err != nil {
		return
	}
	s.sig = fileSig{size: fi.Size(), mod: fi.ModTime()}
	s.haveSig = true
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

	// copy-on-write：构造新表后整体替换，失败则回滚。
	newAccounts := append(cloneAccounts(s.accounts), a)
	newByAK := cloneByAK(s.byAK)
	newByAK[a.AK] = a
	oldAccounts, oldByAK := s.accounts, s.byAK
	s.accounts, s.byAK = newAccounts, newByAK
	if err := s.saveLocked(); err != nil {
		s.accounts, s.byAK = oldAccounts, oldByAK
		return nil, err
	}
	s.refreshSigLocked()
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

	na := a.Clone()
	na.SK = sk
	s.replaceLocked(a, na)
	if err := s.saveLocked(); err != nil {
		s.replaceLocked(na, a)
		return "", err
	}
	s.refreshSigLocked()
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

	na := a.Clone()
	na.Disabled = disabled
	s.replaceLocked(a, na)
	if err := s.saveLocked(); err != nil {
		s.replaceLocked(na, a)
		return err
	}
	s.refreshSigLocked()
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

	newAccounts := make([]*Account, 0, len(s.accounts)-1)
	newAccounts = append(newAccounts, s.accounts[:idx]...)
	newAccounts = append(newAccounts, s.accounts[idx+1:]...)
	newByAK := cloneByAK(s.byAK)
	delete(newByAK, removed.AK)

	oldAccounts, oldByAK := s.accounts, s.byAK
	s.accounts, s.byAK = newAccounts, newByAK
	if err := s.saveLocked(); err != nil {
		s.accounts, s.byAK = oldAccounts, oldByAK
		return err
	}
	s.refreshSigLocked()
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

// replaceLocked 用 to 原子地替换 slice 与 AK 索引中的 from。调用方需持有写锁。
func (s *Store) replaceLocked(from, to *Account) {
	for i, a := range s.accounts {
		if a == from {
			s.accounts[i] = to
			break
		}
	}
	if _, ok := s.byAK[from.AK]; ok {
		if to.AK != from.AK {
			delete(s.byAK, from.AK)
		}
		s.byAK[to.AK] = to
	}
}

func cloneAccounts(in []*Account) []*Account {
	out := make([]*Account, len(in))
	copy(out, in)
	return out
}

func cloneByAK(in map[string]*Account) map[string]*Account {
	out := make(map[string]*Account, len(in)+1)
	for k, v := range in {
		out[k] = v
	}
	return out
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
