package backend

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/YLing2024/objbox/internal/randstr"
	"github.com/johannesboyne/gofakes3"
)

// 分片上传实现说明：
//
//   - 分片临时文件落在账号 root 的内部目录 <root>/.objbox/multipart/<uploadId>/，
//     与桶目录（<root>/<bucket>）完全隔离，因此不会被 ListObjects 看到；
//   - 元数据（key/uploadId/分片清单）存 bbolt 的 uploads 桶；
//   - 上传分片时从 io.Reader 直接 io.Copy 落盘，绝不整片读进内存；
//   - CompleteMultipartUpload 逐个校验 ETag 与分片大小，按请求顺序流式拼接；
//   - 最终对象 ETag 取「拼接后完整内容的 MD5 十六进制」，与普通 PutObject 一致
//     （有意不采用 S3 的 "<md5>-<N>" 复合形式，保持 GET/HEAD 的 ETag 语义统一）。
//
// 有意放宽：不强制最小分片 5MiB，接受更小的分片，便于测试；真实 S3 只允许
// 最后一片小于 5MiB，这里宽松接受。
const (
	// multipartDirName 是 root 内部目录下存放分片任务的子目录名。
	multipartDirName = "multipart"
	// multipartExpiry 是未完成分片任务的过期时长（启动时清理）。
	multipartExpiry = 24 * time.Hour
	// uploadIDBytes 是 uploadId 的随机字节数（base32 后约 26 字符）。
	uploadIDBytes = 16
)

// multipartRoot 返回分片临时文件根目录。
func (b *Backend) multipartRoot() string {
	return filepath.Join(b.root, internalDirName, multipartDirName)
}

// uploadDir 返回某个 uploadId 的分片目录。
func (b *Backend) uploadDir(id string) string {
	return filepath.Join(b.multipartRoot(), id)
}

func (b *Backend) partPath(id string, partNumber int) string {
	return filepath.Join(b.uploadDir(id), strconv.Itoa(partNumber))
}

// CreateMultipartUpload 创建分片任务并落 bbolt。
func (b *Backend) CreateMultipartUpload(bucket, object string, meta map[string]string) (gofakes3.UploadID, error) {
	if !b.bucketExists(bucket) {
		return "", gofakes3.BucketNotFound(bucket)
	}
	if err := ValidateKey(object); err != nil {
		return "", invalidPathErr(err)
	}
	id, err := randstr.Base32Upper(uploadIDBytes)
	if err != nil {
		return "", fmt.Errorf("backend: 生成 uploadId 失败: %w", err)
	}
	if err := os.MkdirAll(b.uploadDir(id), dirMode); err != nil {
		return "", fmt.Errorf("backend: 创建分片目录失败: %w", err)
	}
	up := uploadMeta{
		Bucket:      bucket,
		Key:         object,
		UploadID:    id,
		Initiated:   b.now().UTC().UnixNano(),
		ContentType: meta["Content-Type"],
		UserMeta:    userMetaFromHeaders(meta),
	}
	if err := b.meta.PutUpload(up); err != nil {
		os.RemoveAll(b.uploadDir(id))
		return "", err
	}
	return gofakes3.UploadID(id), nil
}

// UploadPart 流式落盘一个分片并返回其带引号 ETag。同一分片重复上传以最后一次为准。
func (b *Backend) UploadPart(bucket, object string, id gofakes3.UploadID, partNumber int, contentLength int64, input io.Reader) (string, error) {
	if partNumber < 1 || partNumber > gofakes3.MaxUploadPartNumber {
		return "", gofakes3.ErrInvalidPart
	}
	up, ok, err := b.meta.GetUpload(bucket, object, string(id))
	if err != nil {
		return "", err
	}
	if !ok {
		return "", gofakes3.ErrNoSuchUpload
	}

	dir := b.uploadDir(up.UploadID)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return "", fmt.Errorf("backend: 创建分片目录失败: %w", err)
	}
	tmp, err := os.CreateTemp(dir, tmpPrefix)
	if err != nil {
		return "", fmt.Errorf("backend: 创建分片临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	h := md5.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), input)
	if err != nil {
		tmp.Close()
		return "", fmt.Errorf("backend: 写入分片失败: %w", err)
	}
	if contentLength >= 0 && n != contentLength {
		tmp.Close()
		return "", gofakes3.ErrIncompleteBody
	}
	if err := tmp.Chmod(fileMode); err != nil {
		tmp.Close()
		return "", fmt.Errorf("backend: 设置分片权限失败: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", fmt.Errorf("backend: fsync 分片失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("backend: 关闭分片临时文件失败: %w", err)
	}
	if err := os.Rename(tmpName, b.partPath(up.UploadID, partNumber)); err != nil {
		return "", fmt.Errorf("backend: 替换分片失败: %w", err)
	}

	etag := hex.EncodeToString(h.Sum(nil))
	if _, ok, err := b.meta.UpdateUpload(bucket, object, string(id), func(u *uploadMeta) error {
		if u.Parts == nil {
			u.Parts = map[int]partMeta{}
		}
		u.Parts[partNumber] = partMeta{
			Size:         n,
			ETag:         etag,
			LastModified: b.now().UTC().UnixNano(),
		}
		return nil
	}); err != nil {
		return "", err
	} else if !ok {
		return "", gofakes3.ErrNoSuchUpload
	}
	return `"` + etag + `"`, nil
}

// ListParts 返回某分片任务已上传的分片清单。
func (b *Backend) ListParts(bucket, object string, uploadID gofakes3.UploadID, marker int, limit int64) (*gofakes3.ListMultipartUploadPartsResult, error) {
	up, ok, err := b.meta.GetUpload(bucket, object, string(uploadID))
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, gofakes3.ErrNoSuchUpload
	}

	numbers := make([]int, 0, len(up.Parts))
	for n := range up.Parts {
		if n >= marker {
			numbers = append(numbers, n)
		}
	}
	sort.Ints(numbers)

	result := &gofakes3.ListMultipartUploadPartsResult{
		Bucket:           bucket,
		Key:              object,
		UploadID:         uploadID,
		PartNumberMarker: marker,
		MaxParts:         limit,
		StorageClass:     gofakes3.StorageStandard,
	}
	for _, n := range numbers {
		if limit > 0 && int64(len(result.Parts)) >= limit {
			result.IsTruncated = true
			result.NextPartNumberMarker = n
			break
		}
		p := up.Parts[n]
		result.Parts = append(result.Parts, gofakes3.ListMultipartUploadPartItem{
			PartNumber:   n,
			ETag:         `"` + p.ETag + `"`,
			Size:         p.Size,
			LastModified: gofakes3.NewContentTime(time.Unix(0, p.LastModified).UTC()),
		})
	}
	return result, nil
}

// ListMultipartUploads 列出某个 bucket 下未完成的分片任务（支持 prefix/delimiter/分页）。
func (b *Backend) ListMultipartUploads(bucket string, marker *gofakes3.UploadListMarker, prefix gofakes3.Prefix, limit int64) (*gofakes3.ListMultipartUploadsResult, error) {
	if !b.bucketExists(bucket) {
		return nil, gofakes3.BucketNotFound(bucket)
	}
	uploads, err := b.meta.ListUploadsByBucket(bucket)
	if err != nil {
		return nil, err
	}
	sort.Slice(uploads, func(i, j int) bool {
		if uploads[i].Key != uploads[j].Key {
			return uploads[i].Key < uploads[j].Key
		}
		return uploads[i].UploadID < uploads[j].UploadID
	})

	result := &gofakes3.ListMultipartUploadsResult{
		Bucket:     bucket,
		Prefix:     prefix.Prefix,
		Delimiter:  prefix.Delimiter,
		MaxUploads: limit,
	}
	if marker != nil {
		result.KeyMarker = marker.Object
		result.UploadIDMarker = marker.UploadID
	}

	seenPrefixes := map[string]bool{}
	var match gofakes3.PrefixMatch
	for _, u := range uploads {
		if !prefix.Match(u.Key, &match) {
			continue
		}
		if marker != nil {
			// 跳过 marker 之前（含）的任务，语义与默认实现保持一致。
			if u.Key < marker.Object {
				continue
			}
			if u.Key == marker.Object && marker.UploadID != "" && u.UploadID <= string(marker.UploadID) {
				continue
			}
		}
		if match.CommonPrefix {
			if !seenPrefixes[match.MatchedPart] {
				seenPrefixes[match.MatchedPart] = true
				result.CommonPrefixes = append(result.CommonPrefixes, match.AsCommonPrefix())
			}
			continue
		}
		if limit > 0 && int64(len(result.Uploads)) >= limit {
			result.IsTruncated = true
			result.NextKeyMarker = u.Key
			result.NextUploadIDMarker = gofakes3.UploadID(u.UploadID)
			break
		}
		result.Uploads = append(result.Uploads, gofakes3.ListMultipartUploadItem{
			Key:          u.Key,
			UploadID:     gofakes3.UploadID(u.UploadID),
			StorageClass: gofakes3.StorageStandard,
			Initiated:    gofakes3.NewContentTime(u.initiatedTime()),
		})
	}
	return result, nil
}

// AbortMultipartUpload 清理分片目录与元数据。
func (b *Backend) AbortMultipartUpload(bucket, object string, id gofakes3.UploadID) error {
	up, ok, err := b.meta.GetUpload(bucket, object, string(id))
	if err != nil {
		return err
	}
	if !ok {
		return gofakes3.ErrNoSuchUpload
	}
	b.mpMu.Lock()
	defer b.mpMu.Unlock()
	if err := os.RemoveAll(b.uploadDir(up.UploadID)); err != nil {
		return fmt.Errorf("backend: 清理分片目录失败: %w", err)
	}
	return b.meta.DeleteUpload(bucket, object, string(id))
}

// CompleteMultipartUpload 校验并拼接分片，生成最终对象。
//
// 逐片校验 ETag 与大小，任何不符返回 InvalidPart；分片号必须严格递增，
// 否则返回 InvalidPartOrder。最终 ETag 为拼接后内容的 MD5。
func (b *Backend) CompleteMultipartUpload(bucket, object string, id gofakes3.UploadID, input *gofakes3.CompleteMultipartUploadRequest) (gofakes3.VersionID, string, error) {
	up, ok, err := b.meta.GetUpload(bucket, object, string(id))
	if err != nil {
		return "", "", err
	}
	if !ok {
		return "", "", gofakes3.ErrNoSuchUpload
	}
	if len(input.Parts) == 0 {
		return "", "", gofakes3.ErrorMessage(gofakes3.ErrInvalidPart, "no parts in complete request")
	}
	last := 0
	for _, p := range input.Parts {
		if p.PartNumber <= last {
			return "", "", gofakes3.ErrInvalidPartOrder
		}
		last = p.PartNumber
	}

	// 先整体校验，避免拼接中途失败留下半成品。
	for _, p := range input.Parts {
		pm, exists := up.Parts[p.PartNumber]
		if !exists {
			return "", "", gofakes3.ErrorMessagef(gofakes3.ErrInvalidPart, "part %d not found", p.PartNumber)
		}
		if strings.Trim(p.ETag, `"`) != pm.ETag {
			return "", "", gofakes3.ErrorMessagef(gofakes3.ErrInvalidPart, "part %d etag mismatch", p.PartNumber)
		}
		fi, err := os.Stat(b.partPath(up.UploadID, p.PartNumber))
		if err != nil || fi.Size() != pm.Size {
			return "", "", gofakes3.ErrorMessagef(gofakes3.ErrInvalidPart, "part %d size mismatch", p.PartNumber)
		}
	}

	b.mpMu.Lock()
	defer b.mpMu.Unlock()

	path, err := SafeJoin(b.root, bucket, object)
	if err != nil {
		return "", "", invalidPathErr(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), dirMode); err != nil {
		return "", "", fmt.Errorf("backend: 创建目录失败: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), tmpPrefix)
	if err != nil {
		return "", "", fmt.Errorf("backend: 创建临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	h := md5.New()
	var total int64
	for _, p := range input.Parts {
		f, err := os.Open(b.partPath(up.UploadID, p.PartNumber))
		if err != nil {
			tmp.Close()
			return "", "", fmt.Errorf("backend: 打开分片失败: %w", err)
		}
		n, err := io.Copy(io.MultiWriter(tmp, h), f)
		f.Close()
		if err != nil {
			tmp.Close()
			return "", "", fmt.Errorf("backend: 拼接分片失败: %w", err)
		}
		total += n
	}
	if err := tmp.Chmod(fileMode); err != nil {
		tmp.Close()
		return "", "", fmt.Errorf("backend: 设置对象权限失败: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", "", fmt.Errorf("backend: fsync 对象失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", "", fmt.Errorf("backend: 关闭临时文件失败: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return "", "", fmt.Errorf("backend: 替换对象失败: %w", err)
	}

	etag := hex.EncodeToString(h.Sum(nil))
	om := objectMeta{
		Size:         total,
		ETag:         etag,
		ContentType:  up.ContentType,
		LastModified: b.now().UTC().UnixNano(),
		UserMeta:     up.UserMeta,
	}
	if err := b.meta.Put(bucket, object, om); err != nil {
		return "", "", err
	}

	if err := os.RemoveAll(b.uploadDir(up.UploadID)); err != nil {
		return "", "", fmt.Errorf("backend: 清理分片目录失败: %w", err)
	}
	if err := b.meta.DeleteUpload(bucket, object, string(id)); err != nil {
		return "", "", err
	}
	return "", `"` + etag + `"`, nil
}

// MultipartTotalSize 返回某分片任务已上传分片的大小之和，供配额预检使用。
func (b *Backend) MultipartTotalSize(bucket, key, id string) (int64, bool, error) {
	up, ok, err := b.meta.GetUpload(bucket, key, id)
	if err != nil || !ok {
		return 0, ok, err
	}
	var total int64
	for _, p := range up.Parts {
		total += p.Size
	}
	return total, true, nil
}

// CleanupExpiredUploads 清理超过 maxAge 未完成的分片任务（含磁盘目录）。
// 返回清理数量。启动时调用一次即可。
func (b *Backend) CleanupExpiredUploads(maxAge time.Duration) (int, error) {
	if maxAge <= 0 {
		maxAge = multipartExpiry
	}
	cutoff := b.now().UTC().Add(-maxAge)
	var expired []uploadMeta
	if err := b.meta.RangeUploads(func(u uploadMeta) error {
		if t := u.initiatedTime(); !t.IsZero() && t.Before(cutoff) {
			expired = append(expired, u)
		}
		return nil
	}); err != nil {
		return 0, err
	}
	for _, u := range expired {
		if err := os.RemoveAll(b.uploadDir(u.UploadID)); err != nil {
			return 0, fmt.Errorf("backend: 清理过期分片目录失败: %w", err)
		}
		if err := b.meta.DeleteUpload(u.Bucket, u.Key, u.UploadID); err != nil {
			return 0, err
		}
	}
	return len(expired), nil
}
