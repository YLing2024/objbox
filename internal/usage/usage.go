// Package usage 统计账号数据目录的递归大小，并带一层时间缓存。
//
// M0 仅用于 CLI 展示；用量配额（quotaBytes）本身尚未参与写入限制。
package usage

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// DirSize 递归统计 dir 下所有普通文件的字节数。目录不存在时返回 0。
func DirSize(dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("usage: 统计 %s 失败: %w", dir, err)
	}
	return total, nil
}

type cacheEntry struct {
	size int64
	at   time.Time
}

// Cache 是带 TTL 的目录大小缓存。
type Cache struct {
	ttl time.Duration

	mu      sync.Mutex
	entries map[string]cacheEntry
}

// NewCache 创建缓存；ttl <= 0 表示每次都重新统计。
func NewCache(ttl time.Duration) *Cache {
	return &Cache{ttl: ttl, entries: map[string]cacheEntry{}}
}

// DirSize 返回 dir 的大小，命中未过期缓存时直接返回。
func (c *Cache) DirSize(dir string) (int64, error) {
	if c == nil || c.ttl <= 0 {
		return DirSize(dir)
	}
	c.mu.Lock()
	if e, ok := c.entries[dir]; ok && time.Since(e.at) < c.ttl {
		c.mu.Unlock()
		return e.size, nil
	}
	c.mu.Unlock()

	size, err := DirSize(dir)
	if err != nil {
		return 0, err
	}
	c.mu.Lock()
	c.entries[dir] = cacheEntry{size: size, at: time.Now()}
	c.mu.Unlock()
	return size, nil
}
