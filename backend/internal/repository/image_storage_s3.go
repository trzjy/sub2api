package repository

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/servertiming"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// s3DeleteBatchSize 是单次 DeleteObjects 的对象数上限（S3 协议约束）。
const s3DeleteBatchSize = 1000

// S3ImageStorage 用 S3 兼容对象存储实现 service.ImageStorage。
type S3ImageStorage struct {
	client        *s3.Client
	bucket        string
	publicBaseURL string
	presignExpiry time.Duration
}

var _ service.ImageStorage = (*S3ImageStorage)(nil)

// NewS3ImageStorage 依据配置构造 S3 图片存储（调用方应先确认 cfg.Active()）。
func NewS3ImageStorage(ctx context.Context, cfg *config.ImageStorageConfig) (*S3ImageStorage, error) {
	client, err := newS3Client(ctx, s3ClientParams{
		Endpoint:        cfg.Endpoint,
		Region:          cfg.Region,
		AccessKeyID:     cfg.AccessKeyID,
		SecretAccessKey: cfg.SecretAccessKey,
		ForcePathStyle:  cfg.ForcePathStyle,
	})
	if err != nil {
		return nil, err
	}

	expiry := time.Duration(cfg.PresignExpiry) * time.Hour
	if expiry <= 0 {
		expiry = 24 * time.Hour
	}

	return &S3ImageStorage{
		client:        client,
		bucket:        cfg.Bucket,
		publicBaseURL: strings.TrimRight(cfg.PublicBaseURL, "/"),
		presignExpiry: expiry,
	}, nil
}

// Save 上传图片字节，返回可访问 URL：配了 public_base_url 则返回公开直链，否则返回 presigned 临时链接。
func (s *S3ImageStorage) Save(ctx context.Context, key, contentType string, data []byte) (string, error) {
	finish := servertiming.ObserveDependency(ctx, "s3")
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      &s.bucket,
		Key:         &key,
		Body:        bytes.NewReader(data),
		ContentType: &contentType,
	})
	finish()
	if err != nil {
		return "", fmt.Errorf("S3 PutObject: %w", err)
	}

	if s.publicBaseURL != "" {
		return s.publicBaseURL + "/" + strings.TrimLeft(key, "/"), nil
	}

	presignClient := s3.NewPresignClient(s.client)
	result, err := presignClient.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: &s.bucket,
		Key:    &key,
	}, s3.WithPresignExpires(s.presignExpiry))
	if err != nil {
		return "", fmt.Errorf("presign url: %w", err)
	}
	return result.URL, nil
}

// DeleteByPrefix 删除指定前缀下的所有对象：ListObjectsV2 按 continuation token 翻页枚举，
// 每页对象用 DeleteObjects（单批 ≤1000 key）批量删除。前缀下无对象返回 (0, nil)（幂等）。
// 只把 DeleteObjects 响应中确认删除的 key 计入 deleted；任何逐对象 Errors 汇总为非空 error
// 返回（尽力而为语义：不做部分失败重试，由调用方决定后续处理）。
func (s *S3ImageStorage) DeleteByPrefix(ctx context.Context, prefix string) (int, error) {
	finish := servertiming.ObserveDependency(ctx, "s3")
	defer finish()

	deleted := 0
	var failedKeys []string
	paginator := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: &s.bucket,
		Prefix: &prefix,
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return deleted, fmt.Errorf("S3 ListObjectsV2 (prefix %q): %w", prefix, err)
		}
		keys := make([]string, 0, len(page.Contents))
		for _, obj := range page.Contents {
			if key := aws.ToString(obj.Key); key != "" {
				keys = append(keys, key)
			}
		}
		for start := 0; start < len(keys); start += s3DeleteBatchSize {
			end := start + s3DeleteBatchSize
			if end > len(keys) {
				end = len(keys)
			}
			batch := keys[start:end]
			if err := s.deleteBatch(ctx, batch, &deleted, &failedKeys); err != nil {
				return deleted, err
			}
		}
	}
	if len(failedKeys) > 0 {
		return deleted, fmt.Errorf("S3 DeleteObjects failed for %d object(s): %s", len(failedKeys), strings.Join(failedKeys, ", "))
	}
	return deleted, nil
}

// deleteBatch 删除一批 key，把确认删除的数量累加进 deleted、失败的 key 追加进 failedKeys。
func (s *S3ImageStorage) deleteBatch(ctx context.Context, keys []string, deleted *int, failedKeys *[]string) error {
	objects := make([]types.ObjectIdentifier, 0, len(keys))
	for _, key := range keys {
		k := key
		objects = append(objects, types.ObjectIdentifier{Key: &k})
	}
	out, err := s.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
		Bucket: &s.bucket,
		Delete: &types.Delete{Objects: objects},
	})
	if err != nil {
		return fmt.Errorf("S3 DeleteObjects: %w", err)
	}
	*deleted += len(out.Deleted)
	for _, delErr := range out.Errors {
		*failedKeys = append(*failedKeys, fmt.Sprintf("%s (%s: %s)",
			aws.ToString(delErr.Key), aws.ToString(delErr.Code), aws.ToString(delErr.Message)))
	}
	return nil
}

// HasObjectsByPrefix 只读探测前缀下是否存在对象（ListObjectsV2 MaxKeys=1）。
// 探测失败返回 error（fail-closed），调用方不得把错误当"无对象"。
func (s *S3ImageStorage) HasObjectsByPrefix(ctx context.Context, prefix string) (bool, error) {
	finish := servertiming.ObserveDependency(ctx, "s3")
	defer finish()

	out, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket:  &s.bucket,
		Prefix:  &prefix,
		MaxKeys: aws.Int32(1),
	})
	if err != nil {
		return false, fmt.Errorf("S3 ListObjectsV2 (prefix %q): %w", prefix, err)
	}
	return len(out.Contents) > 0, nil
}
