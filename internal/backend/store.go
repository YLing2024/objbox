package backend

import (
	"encoding/json"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"
)

// objectMeta 是对象在 bbolt 中的元数据记录。
type objectMeta struct {
	Size         int64             `json:"size"`
	ETag         string            `json:"etag"` // 内容 MD5 的十六进制（不带引号）
	ContentType  string            `json:"contentType,omitempty"`
	LastModified int64             `json:"lastModified"` // UnixNano
	UserMeta     map[string]string `json:"userMeta,omitempty"`
}

func (m objectMeta) modified() time.Time {
	if m.LastModified == 0 {
		return time.Time{}
	}
	return time.Unix(0, m.LastModified).UTC()
}

var metaBucketName = []byte("objects")

// metaStore 封装 bbolt，提供 bucket/key -> objectMeta 的读写。
type metaStore struct {
	db *bolt.DB
}

// openMetaStore 打开（或创建）元数据文件，并确保根 bucket 存在。
func openMetaStore(path string) (*metaStore, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 3 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("backend: 打开元数据库失败: %w", err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(metaBucketName)
		return err
	}); err != nil {
		db.Close()
		return nil, fmt.Errorf("backend: 初始化元数据库失败: %w", err)
	}
	return &metaStore{db: db}, nil
}

func (m *metaStore) Close() error {
	if m == nil || m.db == nil {
		return nil
	}
	return m.db.Close()
}

// metaKey 使用 bucket + NUL + key 组成复合主键，保证按 bucket 前缀有序。
func metaKey(bucket, key string) []byte {
	b := make([]byte, 0, len(bucket)+1+len(key))
	b = append(b, bucket...)
	b = append(b, 0)
	b = append(b, key...)
	return b
}

func bucketPrefix(bucket string) []byte {
	b := make([]byte, 0, len(bucket)+1)
	b = append(b, bucket...)
	b = append(b, 0)
	return b
}

func (m *metaStore) Put(bucket, key string, meta objectMeta) error {
	raw, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("backend: 序列化元数据失败: %w", err)
	}
	return m.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(metaBucketName).Put(metaKey(bucket, key), raw)
	})
}

func (m *metaStore) Get(bucket, key string) (objectMeta, bool, error) {
	var out objectMeta
	var found bool
	err := m.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket(metaBucketName).Get(metaKey(bucket, key))
		if raw == nil {
			return nil
		}
		found = true
		return json.Unmarshal(raw, &out)
	})
	if err != nil {
		return objectMeta{}, false, fmt.Errorf("backend: 读取元数据失败: %w", err)
	}
	return out, found, nil
}

func (m *metaStore) Delete(bucket, key string) error {
	return m.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(metaBucketName).Delete(metaKey(bucket, key))
	})
}

// DeleteBucket 删除某个 bucket 前缀下的全部元数据。
func (m *metaStore) DeleteBucket(bucket string) error {
	pfx := bucketPrefix(bucket)
	return m.db.Update(func(tx *bolt.Tx) error {
		c := tx.Bucket(metaBucketName).Cursor()
		var keys [][]byte
		for k, _ := c.Seek(pfx); k != nil && hasPrefix(k, pfx); k, _ = c.Next() {
			keys = append(keys, append([]byte(nil), k...))
		}
		for _, k := range keys {
			if err := tx.Bucket(metaBucketName).Delete(k); err != nil {
				return err
			}
		}
		return nil
	})
}

// Range 遍历全部元数据。fn 返回非 nil 会终止遍历。
func (m *metaStore) Range(fn func(bucket, key string, meta objectMeta) error) error {
	return m.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(metaBucketName).ForEach(func(k, v []byte) error {
			bucket, key, ok := splitMetaKey(k)
			if !ok {
				return nil
			}
			var meta objectMeta
			if err := json.Unmarshal(v, &meta); err != nil {
				return err
			}
			return fn(bucket, key, meta)
		})
	})
}

func hasPrefix(b, pfx []byte) bool {
	if len(b) < len(pfx) {
		return false
	}
	for i := range pfx {
		if b[i] != pfx[i] {
			return false
		}
	}
	return true
}

func splitMetaKey(k []byte) (bucket, key string, ok bool) {
	for i, c := range k {
		if c == 0 {
			return string(k[:i]), string(k[i+1:]), true
		}
	}
	return "", "", false
}
