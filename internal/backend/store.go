package backend

import (
	"encoding/json"
	"errors"
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

// partMeta 是分片上传中单个分片的元数据。
type partMeta struct {
	Size         int64  `json:"size"`
	ETag         string `json:"etag"` // 分片内容的 MD5 十六进制（不带引号）
	LastModified int64  `json:"lastModified"`
}

// uploadMeta 是一个未完成分片任务在 bbolt 中的记录。
//
// 分片文件本体放在账号 root 的 .objbox/multipart/<uploadId>/ 下，
// 这里只保存定位与清单信息，避免把大文件读进内存。
type uploadMeta struct {
	Bucket      string            `json:"bucket"`
	Key         string            `json:"key"`
	UploadID    string            `json:"uploadId"`
	Initiated   int64             `json:"initiated"` // UnixNano
	ContentType string            `json:"contentType,omitempty"`
	UserMeta    map[string]string `json:"userMeta,omitempty"`
	Parts       map[int]partMeta  `json:"parts,omitempty"`
}

func (u uploadMeta) initiatedTime() time.Time {
	if u.Initiated == 0 {
		return time.Time{}
	}
	return time.Unix(0, u.Initiated).UTC()
}

var (
	metaBucketName    = []byte("objects")
	uploadsBucketName = []byte("uploads")
)

// metaStore 封装 bbolt，提供 bucket/key -> objectMeta 的读写。
type metaStore struct {
	db *bolt.DB
}

// openMetaStore 打开（或创建）元数据文件，并确保根 bucket 存在。
//
// dataDir 仅用于在元数据库被其它进程锁定时给出可读的错误信息。
func openMetaStore(path, dataDir string) (*metaStore, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 3 * time.Second})
	if err != nil {
		// bbolt 在超时未拿到文件锁时返回 ErrTimeout；归一为“目录已被占用”。
		if errors.Is(err, bolt.ErrTimeout) {
			return nil, fmt.Errorf("backend: 数据目录 %s 已被另一个 objbox 进程占用（元数据库被锁定）", dataDir)
		}
		return nil, fmt.Errorf("backend: 打开元数据库失败: %w", err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		if _, err := tx.CreateBucketIfNotExists(metaBucketName); err != nil {
			return err
		}
		_, err := tx.CreateBucketIfNotExists(uploadsBucketName)
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

// ---- 分片上传元数据 ----

// uploadKey 用 bucket + NUL + key + NUL + uploadId 组成主键，
// 既能按 bucket 前缀扫描，也能精确定位单个 uploadId。
func uploadKey(bucket, key, id string) []byte {
	b := make([]byte, 0, len(bucket)+len(key)+len(id)+2)
	b = append(b, bucket...)
	b = append(b, 0)
	b = append(b, key...)
	b = append(b, 0)
	b = append(b, id...)
	return b
}

func uploadBucketPrefix(bucket string) []byte {
	b := make([]byte, 0, len(bucket)+1)
	b = append(b, bucket...)
	b = append(b, 0)
	return b
}

func (m *metaStore) PutUpload(u uploadMeta) error {
	raw, err := json.Marshal(u)
	if err != nil {
		return fmt.Errorf("backend: 序列化分片元数据失败: %w", err)
	}
	return m.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(uploadsBucketName).Put(uploadKey(u.Bucket, u.Key, u.UploadID), raw)
	})
}

func (m *metaStore) GetUpload(bucket, key, id string) (uploadMeta, bool, error) {
	var out uploadMeta
	var found bool
	err := m.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket(uploadsBucketName).Get(uploadKey(bucket, key, id))
		if raw == nil {
			return nil
		}
		found = true
		return json.Unmarshal(raw, &out)
	})
	if err != nil {
		return uploadMeta{}, false, fmt.Errorf("backend: 读取分片元数据失败: %w", err)
	}
	return out, found, nil
}

// UpdateUpload 在单个 bbolt 事务内完成「读-改-写」，保证并发上传同一 uploadId
// 的不同分片时元数据更新串行且不丢更新。upload 不存在时返回 ok=false。
func (m *metaStore) UpdateUpload(bucket, key, id string, fn func(*uploadMeta) error) (uploadMeta, bool, error) {
	var out uploadMeta
	var found bool
	err := m.db.Update(func(tx *bolt.Tx) error {
		bkt := tx.Bucket(uploadsBucketName)
		k := uploadKey(bucket, key, id)
		raw := bkt.Get(k)
		if raw == nil {
			return nil
		}
		found = true
		if err := json.Unmarshal(raw, &out); err != nil {
			return err
		}
		if err := fn(&out); err != nil {
			return err
		}
		updated, err := json.Marshal(out)
		if err != nil {
			return err
		}
		return bkt.Put(k, updated)
	})
	if err != nil {
		return uploadMeta{}, false, fmt.Errorf("backend: 更新分片元数据失败: %w", err)
	}
	return out, found, nil
}

func (m *metaStore) DeleteUpload(bucket, key, id string) error {
	return m.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(uploadsBucketName).Delete(uploadKey(bucket, key, id))
	})
}

// ListUploadsByBucket 返回某个 bucket 下全部未完成分片任务。
func (m *metaStore) ListUploadsByBucket(bucket string) ([]uploadMeta, error) {
	pfx := uploadBucketPrefix(bucket)
	var out []uploadMeta
	err := m.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(uploadsBucketName).Cursor()
		for k, v := c.Seek(pfx); k != nil && hasPrefix(k, pfx); k, v = c.Next() {
			var u uploadMeta
			if err := json.Unmarshal(v, &u); err != nil {
				return err
			}
			out = append(out, u)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("backend: 扫描分片元数据失败: %w", err)
	}
	return out, nil
}

// RangeUploads 遍历全部未完成分片任务，供启动时过期清理使用。
func (m *metaStore) RangeUploads(fn func(uploadMeta) error) error {
	return m.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(uploadsBucketName).ForEach(func(_, v []byte) error {
			var u uploadMeta
			if err := json.Unmarshal(v, &u); err != nil {
				return err
			}
			return fn(u)
		})
	})
}
