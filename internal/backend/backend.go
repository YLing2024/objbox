// Package backend 实现基于 POSIX 文件系统 + bbolt 元数据的 S3 后端。
//
// 每个账号有独立的 Backend（root 固定为账号 root），因此账号之间天然隔离：
// 一个 Backend 只能看到自己 root 下的桶与对象。
//
// 对象内容直接落盘，元数据（ETag/Content-Type/自定义元数据/最后修改时间）
// 记录在 root/.objbox/meta.db（bbolt）。写入走“临时文件 → fsync → rename”，
// 保证读者永远看不到半个对象。
package backend

import (
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/johannesboyne/gofakes3"
)

const (
	// internalDirName 是账号 root 下的内部目录，存放元数据库，不参与列表。
	internalDirName = ".objbox"
	// metaDBName 是元数据库文件名。
	metaDBName = "meta.db"
	// tmpPrefix 是写入对象时的临时文件前缀，列表与重建索引时跳过。
	tmpPrefix = ".objbox-tmp-"
	// dirMode 是内部目录权限。
	dirMode = 0o700
	// fileMode 是对象文件权限。
	fileMode = 0o644
)

// Backend 是单个账号的存储后端。
type Backend struct {
	root string
	meta *metaStore
	now  func() time.Time
}

// New 打开（或创建）root 下的存储后端，并在启动时用磁盘内容重建/修正索引。
func New(root string) (*Backend, error) {
	if root == "" {
		return nil, errors.New("backend: root 不能为空")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("backend: 解析 root 失败: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(abs, internalDirName), dirMode); err != nil {
		return nil, fmt.Errorf("backend: 创建内部目录失败: %w", err)
	}
	meta, err := openMetaStore(filepath.Join(abs, internalDirName, metaDBName))
	if err != nil {
		return nil, err
	}
	b := &Backend{root: abs, meta: meta, now: time.Now}
	if err := b.Reconcile(); err != nil {
		meta.Close()
		return nil, err
	}
	return b, nil
}

// Close 关闭元数据库。
func (b *Backend) Close() error { return b.meta.Close() }

// Root 返回账号隔离根目录。
func (b *Backend) Root() string { return b.root }

func (b *Backend) bucketDir(bucket string) string {
	return filepath.Join(b.root, bucket)
}

func (b *Backend) bucketExists(bucket string) bool {
	if ValidateBucket(bucket) != nil {
		return false
	}
	fi, err := os.Stat(b.bucketDir(bucket))
	return err == nil && fi.IsDir()
}

// ---- gofakes3.Backend 实现 ----

// ListBuckets 列出账号 root 下所有合法目录作为桶。
func (b *Backend) ListBuckets() ([]gofakes3.BucketInfo, error) {
	entries, err := os.ReadDir(b.root)
	if err != nil {
		return nil, fmt.Errorf("backend: 读取 root 失败: %w", err)
	}
	out := make([]gofakes3.BucketInfo, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if ValidateBucket(name) != nil {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, gofakes3.BucketInfo{
			Name:         name,
			CreationDate: gofakes3.NewContentTime(info.ModTime().UTC()),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// CreateBucket 创建一个空桶。已存在返回 BucketAlreadyExists。
func (b *Backend) CreateBucket(name string) error {
	if err := ValidateBucket(name); err != nil {
		return gofakes3.ErrorMessage(gofakes3.ErrInvalidBucketName, err.Error())
	}
	if err := os.Mkdir(b.bucketDir(name), dirMode); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return gofakes3.ResourceError(gofakes3.ErrBucketAlreadyExists, name)
		}
		return fmt.Errorf("backend: 创建桶失败: %w", err)
	}
	return nil
}

// BucketExists 判断桶是否存在。非法桶名按不存在处理。
func (b *Backend) BucketExists(name string) (bool, error) {
	return b.bucketExists(name), nil
}

// DeleteBucket 删除空桶；非空返回 BucketNotEmpty，不存在返回 NoSuchBucket。
func (b *Backend) DeleteBucket(name string) error {
	if !b.bucketExists(name) {
		return gofakes3.BucketNotFound(name)
	}
	empty, err := b.bucketEmpty(name)
	if err != nil {
		return err
	}
	if !empty {
		return gofakes3.ResourceError(gofakes3.ErrBucketNotEmpty, name)
	}
	if err := os.RemoveAll(b.bucketDir(name)); err != nil {
		return fmt.Errorf("backend: 删除桶失败: %w", err)
	}
	return b.meta.DeleteBucket(name)
}

// ForceDeleteBucket 删除桶及其全部内容（仅用于测试/强制删除）。
func (b *Backend) ForceDeleteBucket(name string) error {
	if err := os.RemoveAll(b.bucketDir(name)); err != nil {
		return fmt.Errorf("backend: 强制删除桶失败: %w", err)
	}
	return b.meta.DeleteBucket(name)
}

// bucketEmpty 判断桶内是否没有任何对象文件。
func (b *Backend) bucketEmpty(name string) (bool, error) {
	empty := true
	err := filepath.WalkDir(b.bucketDir(name), func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && !strings.HasPrefix(d.Name(), tmpPrefix) {
			empty = false
			return fs.SkipAll
		}
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return true, nil
	}
	return empty, err
}

// PutObject 原子写入对象：临时文件 → fsync → rename，随后写元数据。
func (b *Backend) PutObject(bucket, key string, meta map[string]string, input io.Reader, size int64, conditions *gofakes3.PutConditions) (gofakes3.PutObjectResult, error) {
	path, err := SafeJoin(b.root, bucket, key)
	if err != nil {
		return gofakes3.PutObjectResult{}, invalidPathErr(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), dirMode); err != nil {
		return gofakes3.PutObjectResult{}, fmt.Errorf("backend: 创建目录失败: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), tmpPrefix)
	if err != nil {
		return gofakes3.PutObjectResult{}, fmt.Errorf("backend: 创建临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	h := md5.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), input)
	if err != nil {
		tmp.Close()
		return gofakes3.PutObjectResult{}, fmt.Errorf("backend: 写入对象失败: %w", err)
	}
	if err := tmp.Chmod(fileMode); err != nil {
		tmp.Close()
		return gofakes3.PutObjectResult{}, fmt.Errorf("backend: 设置对象权限失败: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return gofakes3.PutObjectResult{}, fmt.Errorf("backend: fsync 对象失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return gofakes3.PutObjectResult{}, fmt.Errorf("backend: 关闭临时文件失败: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return gofakes3.PutObjectResult{}, fmt.Errorf("backend: 替换对象失败: %w", err)
	}

	now := b.now().UTC()
	om := objectMeta{
		Size:         n,
		ETag:         hex.EncodeToString(h.Sum(nil)),
		ContentType:  meta["Content-Type"],
		LastModified: now.UnixNano(),
		UserMeta:     userMetaFromHeaders(meta),
	}
	if err := b.meta.Put(bucket, key, om); err != nil {
		return gofakes3.PutObjectResult{}, err
	}
	return gofakes3.PutObjectResult{}, nil
}

// GetObject 读取对象，支持基础 Range。
func (b *Backend) GetObject(bucket, key string, rangeRequest *gofakes3.ObjectRangeRequest) (*gofakes3.Object, error) {
	path, err := SafeJoin(b.root, bucket, key)
	if err != nil {
		return nil, invalidPathErr(err)
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, gofakes3.KeyNotFound(key)
		}
		return nil, fmt.Errorf("backend: 打开对象失败: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("backend: 读取对象状态失败: %w", err)
	}

	md, _, err := b.meta.Get(bucket, key)
	if err != nil {
		f.Close()
		return nil, err
	}
	etag := md.ETag
	if etag == "" {
		etag, err = md5File(path)
		if err != nil {
			f.Close()
			return nil, err
		}
	}
	hash, _ := hex.DecodeString(etag)

	obj := &gofakes3.Object{
		Name:     key,
		Metadata: responseMetadata(md, info.ModTime()),
		Size:     info.Size(),
		Hash:     hash,
		Contents: f,
	}

	if rangeRequest != nil {
		rng, err := rangeRequest.Range(info.Size())
		if err != nil {
			f.Close()
			return nil, err
		}
		if rng != nil {
			if _, err := f.Seek(rng.Start, io.SeekStart); err != nil {
				f.Close()
				return nil, fmt.Errorf("backend: 定位 Range 失败: %w", err)
			}
			obj.Range = rng
			obj.Contents = &sectionReadCloser{Reader: io.LimitReader(f, rng.Length), Closer: f}
		}
	}
	return obj, nil
}

// HeadObject 返回对象元信息，Contents 为立即 EOF 的空读取器。
func (b *Backend) HeadObject(bucket, key string) (*gofakes3.Object, error) {
	path, err := SafeJoin(b.root, bucket, key)
	if err != nil {
		return nil, invalidPathErr(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, gofakes3.KeyNotFound(key)
		}
		return nil, fmt.Errorf("backend: 读取对象状态失败: %w", err)
	}
	md, _, err := b.meta.Get(bucket, key)
	if err != nil {
		return nil, err
	}
	etag := md.ETag
	if etag == "" {
		etag, err = md5File(path)
		if err != nil {
			return nil, err
		}
	}
	hash, _ := hex.DecodeString(etag)
	return &gofakes3.Object{
		Name:     key,
		Metadata: responseMetadata(md, info.ModTime()),
		Size:     info.Size(),
		Hash:     hash,
		Contents: io.NopCloser(strings.NewReader("")),
	}, nil
}

// DeleteObject 删除对象与元数据；对象不存在时视为成功（S3 语义）。
func (b *Backend) DeleteObject(bucket, key string) (gofakes3.ObjectDeleteResult, error) {
	if !b.bucketExists(bucket) {
		return gofakes3.ObjectDeleteResult{}, gofakes3.BucketNotFound(bucket)
	}
	path, err := SafeJoin(b.root, bucket, key)
	if err != nil {
		return gofakes3.ObjectDeleteResult{}, invalidPathErr(err)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return gofakes3.ObjectDeleteResult{}, fmt.Errorf("backend: 删除对象失败: %w", err)
	}
	if err := b.meta.Delete(bucket, key); err != nil {
		return gofakes3.ObjectDeleteResult{}, err
	}
	pruneEmptyDirs(filepath.Dir(path), b.bucketDir(bucket))
	return gofakes3.ObjectDeleteResult{}, nil
}

// ListBucket 实现 prefix + delimiter + marker + max-keys 分页列表。
func (b *Backend) ListBucket(name string, prefix *gofakes3.Prefix, page gofakes3.ListBucketPage) (*gofakes3.ObjectList, error) {
	if !b.bucketExists(name) {
		return nil, gofakes3.BucketNotFound(name)
	}
	entries, err := b.listEntries(name)
	if err != nil {
		return nil, err
	}

	out := gofakes3.NewObjectList()
	maxKeys := int(page.MaxKeys)
	if maxKeys == 0 {
		// max-keys=0：返回空列表且不标记截断。
		return out, nil
	}
	truncated := false
	lastKey := ""
	count := 0
	seenPrefix := map[string]bool{}

	for _, e := range entries {
		if prefix != nil && prefix.HasPrefix && !strings.HasPrefix(e.key, prefix.Prefix) {
			continue
		}
		if page.HasMarker && e.key <= page.Marker {
			continue
		}

		// delimiter 归并为 CommonPrefixes
		if prefix != nil && prefix.HasDelimiter && prefix.Delimiter != "" {
			rest := strings.TrimPrefix(e.key, prefix.Prefix)
			if i := strings.Index(rest, prefix.Delimiter); i >= 0 {
				cp := prefix.Prefix + rest[:i+len(prefix.Delimiter)]
				if !seenPrefix[cp] {
					if count >= maxKeys {
						truncated = true
						break
					}
					seenPrefix[cp] = true
					out.AddPrefix(cp)
					count++
					lastKey = cp
				}
				continue
			}
		}

		if count >= maxKeys {
			truncated = true
			break
		}
		out.Add(&gofakes3.Content{
			Key:          e.key,
			LastModified: gofakes3.NewContentTime(e.lastModified),
			ETag:         `"` + e.etag + `"`,
			Size:         e.size,
		})
		count++
		lastKey = e.key
	}

	if truncated {
		out.IsTruncated = true
		out.NextMarker = lastKey
	}
	return out, nil
}

// DeleteMulti 未实现（M0 范围之外，批量删属 M1）。
func (b *Backend) DeleteMulti(_ string, _ ...string) (gofakes3.MultiDeleteResult, error) {
	return gofakes3.MultiDeleteResult{}, gofakes3.ErrNotImplemented
}

// CopyObject 未实现（M0 范围之外，CopyObject 属 M1）。
func (b *Backend) CopyObject(_, _, _, _ string, _ map[string]string) (gofakes3.CopyObjectResult, error) {
	return gofakes3.CopyObjectResult{}, gofakes3.ErrNotImplemented
}

// ---- 列表与重建 ----

type objectEntry struct {
	key          string
	size         int64
	lastModified time.Time
	etag         string
}

func (b *Backend) listEntries(bucket string) ([]objectEntry, error) {
	dir := b.bucketDir(bucket)
	var out []objectEntry
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasPrefix(d.Name(), tmpPrefix) {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		md, ok, err := b.meta.Get(bucket, key)
		if err != nil {
			return err
		}
		etag := md.ETag
		if !ok || etag == "" {
			etag, err = md5File(p)
			if err != nil {
				return err
			}
		}
		out = append(out, objectEntry{
			key:          key,
			size:         info.Size(),
			lastModified: info.ModTime().UTC(),
			etag:         etag,
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("backend: 遍历桶失败: %w", err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key < out[j].key })
	return out, nil
}

// Reconcile 以磁盘为准重建索引：
//   - 磁盘有文件但元数据缺失/大小不符 → 补齐（重算 MD5）；
//   - 元数据存在但文件已消失 → 删除该元数据。
func (b *Backend) Reconcile() error {
	disk := map[string]struct{}{}

	bucketEntries, err := os.ReadDir(b.root)
	if err != nil {
		return fmt.Errorf("backend: 读取 root 失败: %w", err)
	}
	for _, be := range bucketEntries {
		if !be.IsDir() || strings.HasPrefix(be.Name(), ".") {
			continue
		}
		bucket := be.Name()
		if ValidateBucket(bucket) != nil {
			continue
		}
		dir := b.bucketDir(bucket)
		err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || strings.HasPrefix(d.Name(), tmpPrefix) {
				return nil
			}
			rel, err := filepath.Rel(dir, p)
			if err != nil {
				return err
			}
			key := filepath.ToSlash(rel)
			disk[bucket+"\x00"+key] = struct{}{}

			info, err := d.Info()
			if err != nil {
				return err
			}
			md, ok, err := b.meta.Get(bucket, key)
			if err != nil {
				return err
			}
			if ok && md.Size == info.Size() && md.ETag != "" {
				return nil
			}
			etag, err := md5File(p)
			if err != nil {
				return err
			}
			return b.meta.Put(bucket, key, objectMeta{
				Size:         info.Size(),
				ETag:         etag,
				ContentType:  md.ContentType,
				LastModified: info.ModTime().UnixNano(),
				UserMeta:     md.UserMeta,
			})
		})
		if err != nil {
			return fmt.Errorf("backend: 重建索引失败: %w", err)
		}
	}

	var stale [][2]string
	if err := b.meta.Range(func(bucket, key string, _ objectMeta) error {
		if _, ok := disk[bucket+"\x00"+key]; !ok {
			stale = append(stale, [2]string{bucket, key})
		}
		return nil
	}); err != nil {
		return fmt.Errorf("backend: 扫描元数据失败: %w", err)
	}
	for _, s := range stale {
		if err := b.meta.Delete(s[0], s[1]); err != nil {
			return err
		}
	}
	return nil
}

// ---- 辅助 ----

type sectionReadCloser struct {
	io.Reader
	io.Closer
}

func invalidPathErr(err error) error {
	var pe *PathError
	if errors.As(err, &pe) {
		return gofakes3.ErrorMessage(gofakes3.ErrInvalidArgument, pe.Reason)
	}
	return gofakes3.ErrorMessage(gofakes3.ErrInvalidArgument, err.Error())
}

func md5File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("backend: 打开对象失败: %w", err)
	}
	defer f.Close()
	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("backend: 计算 MD5 失败: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func userMetaFromHeaders(headers map[string]string) map[string]string {
	var out map[string]string
	for k, v := range headers {
		if strings.HasPrefix(strings.ToLower(k), "x-amz-meta-") {
			if out == nil {
				out = map[string]string{}
			}
			out[k] = v
		}
	}
	return out
}

func responseMetadata(md objectMeta, modTime time.Time) map[string]string {
	out := map[string]string{}
	lm := md.modified()
	if lm.IsZero() {
		lm = modTime.UTC()
	}
	if !lm.IsZero() {
		out["Last-Modified"] = lm.UTC().Format(http.TimeFormat)
	}
	if md.ContentType != "" {
		out["Content-Type"] = md.ContentType
	}
	for k, v := range md.UserMeta {
		out[k] = v
	}
	return out
}

// pruneEmptyDirs 删除 path 到 stop（不含）之间所有已空的目录。
func pruneEmptyDirs(path, stop string) {
	path = filepath.Clean(path)
	stop = filepath.Clean(stop)
	for path != stop && strings.HasPrefix(path, stop+string(filepath.Separator)) {
		entries, err := os.ReadDir(path)
		if err != nil || len(entries) > 0 {
			return
		}
		if err := os.Remove(path); err != nil {
			return
		}
		path = filepath.Dir(path)
	}
}
