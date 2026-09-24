package r2

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"ai-business-service/internal/biz/r2audit"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsv4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

func TestClientPut默认公开桶并沿用媒体不可变缓存(t *testing.T) {
	api := &recordingS3{}
	client := newClientForTest(t, api)
	_, err := client.Put(context.Background(), PutInput{
		Key: "images/result.png", Body: strings.NewReader("result"), ContentType: "image/png",
	})
	if err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	if api.put == nil || aws.ToString(api.put.Bucket) != "cling-ai" || aws.ToString(api.put.Key) != "images/result.png" {
		t.Fatalf("PutObject input = %#v", api.put)
	}
	if got := aws.ToString(api.put.CacheControl); got != immutableMediaCacheControl {
		t.Fatalf("CacheControl = %q, want frozen immutable media header", got)
	}
}

func TestClientPutImmutable按冻结契约写入并全量读回校验(t *testing.T) {
	content := []byte("verified material")
	contentSum := sha256.Sum256(content)
	contentSHA256 := hex.EncodeToString(contentSum[:])
	tenantSHA256 := strings.Repeat("a", 64)
	api := &recordingS3{
		headOutput: &s3.HeadObjectOutput{ContentLength: aws.Int64(int64(len(content)))},
		getOutput:  &s3.GetObjectOutput{Body: io.NopCloser(strings.NewReader(string(content))), ContentLength: aws.Int64(int64(len(content)))},
	}
	client := newClientForTest(t, api)

	info, err := client.PutImmutable(context.Background(), ImmutablePutInput{
		Key:           "results/" + tenantSHA256 + "/" + contentSHA256 + ".png",
		Body:          strings.NewReader(string(content)),
		ContentType:   "image/png",
		ContentLength: int64(len(content)),
		ContentSHA256: contentSHA256,
		TenantSHA256:  tenantSHA256,
	})
	if err != nil {
		t.Fatalf("PutImmutable() error = %v", err)
	}
	if !info.Exists || api.put == nil || api.head == nil || api.get == nil {
		t.Fatalf("PutImmutable calls/info = %#v / put=%#v / head=%#v / get=%#v", info, api.put, api.head, api.get)
	}
	if got := aws.ToString(api.put.IfNoneMatch); got != "*" {
		t.Fatalf("IfNoneMatch = %q, want *", got)
	}
	if got, want := aws.ToString(api.put.ChecksumSHA256), base64.StdEncoding.EncodeToString(contentSum[:]); got != want {
		t.Fatalf("ChecksumSHA256 = %q, want %q", got, want)
	}
	if got := aws.ToInt64(api.put.ContentLength); got != int64(len(content)) {
		t.Fatalf("ContentLength = %d, want %d", got, len(content))
	}
	if got := api.put.Metadata["content-sha256"]; got != contentSHA256 {
		t.Fatalf("content-sha256 metadata = %q", got)
	}
	if got := api.put.Metadata["tenant-sha256"]; got != tenantSHA256 {
		t.Fatalf("tenant-sha256 metadata = %q", got)
	}
	if got := aws.ToString(api.put.ContentType); got != "image/png" {
		t.Fatalf("ContentType = %q", got)
	}
}

func TestClientDeleteMany分批去重且部分删除失败不伪装成功(t *testing.T) {
	api := &recordingS3{deleteManyOutput: &s3.DeleteObjectsOutput{Errors: []types.Error{{Key: aws.String("b"), Code: aws.String("InternalError")}}}}
	client := newClientForTest(t, api)
	_, err := client.DeleteMany(context.Background(), "", []string{"a", "b", "a"})
	if !errors.Is(err, ErrBatchDeleteIncomplete) {
		t.Fatalf("DeleteMany() error = %v, want ErrBatchDeleteIncomplete", err)
	}
	if len(api.deleteMany) != 1 || len(api.deleteMany[0].Delete.Objects) != 2 {
		t.Fatalf("DeleteObjects input = %#v", api.deleteMany)
	}
}

func TestClientCopy保留路径分段并替换元数据(t *testing.T) {
	api := &recordingS3{}
	client := newClientForTest(t, api)
	_, err := client.Copy(context.Background(), CopyInput{
		SourceBucket: "cling-ai", SourceKey: "images/a b.png",
		DestinationBucket: "cling-ai-private", DestinationKey: "ugc/user-1/a.png",
		ContentType: "image/png",
	})
	if err != nil {
		t.Fatalf("Copy() error = %v", err)
	}
	if api.copy == nil {
		t.Fatal("CopyObject not called")
	}
	if got := aws.ToString(api.copy.CopySource); got != "cling-ai/images/a%20b.png" {
		t.Fatalf("CopySource = %q", got)
	}
	if api.copy.MetadataDirective != types.MetadataDirectiveReplace || aws.ToString(api.copy.CacheControl) != immutableMediaCacheControl {
		t.Fatalf("CopyObject metadata/cache = %#v", api.copy)
	}
}

func TestClientPresign传递有效期和下载响应覆盖(t *testing.T) {
	presigner := &recordingPresigner{}
	client := newClientForTest(t, &recordingS3{}, presigner)
	_, err := client.PresignGet(context.Background(), PresignGetInput{
		Key: "videos/clip.mp4", Expires: 5 * time.Minute, ResponseContentDisposition: "attachment; filename=clip.mp4",
	})
	if err != nil {
		t.Fatalf("PresignGet() error = %v", err)
	}
	if presigner.get == nil || aws.ToString(presigner.get.Bucket) != "cling-ai" || aws.ToString(presigner.get.ResponseContentDisposition) != "attachment; filename=clip.mp4" || presigner.getExpires != 5*time.Minute {
		t.Fatalf("PresignGet input = %#v / expires=%s", presigner.get, presigner.getExpires)
	}
}

func TestAuditInventory只列出受管前缀并生成规范公开存储键(t *testing.T) {
	api := &recordingS3{listOutput: &s3.ListObjectsV2Output{
		Contents: []types.Object{
			{Key: aws.String("gen/text_to_image/tenant-a/step-1.png")},
			{Key: aws.String("gen/image_to_video/tenant-a/step-2.mp4")},
		},
		IsTruncated:           aws.Bool(true),
		NextContinuationToken: aws.String("cursor-next"),
	}}
	inventory := NewAuditInventory(newClientForTest(t, api))

	page, err := inventory.List(context.Background(), r2audit.ManagedResultPrefix, "cursor-current", 25)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if api.list == nil || aws.ToString(api.list.Bucket) != "cling-ai" || aws.ToString(api.list.Prefix) != r2audit.ManagedResultPrefix ||
		aws.ToString(api.list.ContinuationToken) != "cursor-current" || aws.ToInt32(api.list.MaxKeys) != 25 {
		t.Fatalf("ListObjectsV2 input = %#v", api.list)
	}
	if page.NextCursor != "cursor-next" || len(page.Objects) != 2 ||
		page.Objects[0].StorageKey != "https://media.example.test/gen/text_to_image/tenant-a/step-1.png" ||
		page.Objects[1].StorageKey != "https://media.example.test/gen/image_to_video/tenant-a/step-2.mp4" {
		t.Fatalf("audit page = %#v", page)
	}
}

func TestClientStreamImmutable边流边分片并在Complete时条件写(t *testing.T) {
	content := bytes.Repeat([]byte("a"), 2*immutableStreamPartSize+7)
	contentSum := sha256.Sum256(content)
	tenantSHA256 := strings.Repeat("a", 64)
	api := &recordingS3{
		headOutput: &s3.HeadObjectOutput{ContentLength: aws.Int64(int64(len(content)))},
		getOutput:  &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(content)), ContentLength: aws.Int64(int64(len(content)))},
	}
	client := newClientForTest(t, api)

	committed, err := client.StreamImmutable(context.Background(), ImmutableStreamInput{
		Key: "gen/text_to_image/" + tenantSHA256 + "/step-1.png", ContentType: "image/png",
		TenantSHA256: tenantSHA256, MaxBytes: int64(len(content)) + 1,
	}, bytes.NewReader(content))
	if err != nil {
		t.Fatalf("StreamImmutable() error = %v", err)
	}
	if committed.Key != "gen/text_to_image/"+tenantSHA256+"/step-1.png" ||
		committed.ContentSHA256 != hex.EncodeToString(contentSum[:]) || committed.ContentLength != int64(len(content)) {
		t.Fatalf("committed = %#v", committed)
	}
	if api.createMultipart == nil || aws.ToString(api.createMultipart.Bucket) != "cling-ai" ||
		aws.ToString(api.createMultipart.CacheControl) != immutableMediaCacheControl ||
		api.createMultipart.Metadata["tenant-sha256"] != tenantSHA256 {
		t.Fatalf("CreateMultipartUpload input = %#v", api.createMultipart)
	}
	// 分片是并发上传的，到达顺序不确定；断言必须按 PartNumber 索引，不能按下标。
	lengthByPart := map[int32]int64{}
	for _, part := range api.uploadParts {
		number := aws.ToInt32(part.PartNumber)
		if _, duplicate := lengthByPart[number]; duplicate {
			t.Fatalf("part %d uploaded more than once", number)
		}
		if aws.ToString(part.Bucket) != "cling-ai" || aws.ToString(part.UploadId) != "upload-1" {
			t.Fatalf("UploadPart input = %#v", part)
		}
		lengthByPart[number] = aws.ToInt64(part.ContentLength)
	}
	if len(lengthByPart) != 3 {
		t.Fatalf("UploadPart count = %d, want 3 parts for %d bytes", len(lengthByPart), len(content))
	}
	if lengthByPart[1] != immutableStreamPartSize || lengthByPart[2] != immutableStreamPartSize || lengthByPart[3] != 7 {
		t.Fatalf("part lengths = %#v, want two full parts and a 7-byte tail", lengthByPart)
	}
	if api.completeMultipart == nil || aws.ToString(api.completeMultipart.IfNoneMatch) != "*" ||
		aws.ToString(api.completeMultipart.UploadId) != "upload-1" {
		t.Fatalf("CompleteMultipartUpload input = %#v", api.completeMultipart)
	}
	// CompleteMultipartUpload 要求分片按 PartNumber 升序，且每个都带 ETag。
	completed := api.completeMultipart.MultipartUpload.Parts
	if len(completed) != 3 {
		t.Fatalf("completed parts = %#v, want 3", completed)
	}
	for index, part := range completed {
		if aws.ToInt32(part.PartNumber) != int32(index+1) || aws.ToString(part.ETag) == "" {
			t.Fatalf("completed part %d = %#v, want ascending numbers with ETags", index, part)
		}
	}
	if api.head == nil || api.get == nil {
		t.Fatal("streamed publish skipped the post-write verification")
	}
	if len(api.abortedUploads) != 0 {
		t.Fatalf("successful stream aborted its upload: %#v", api.abortedUploads)
	}
}

func TestClientStreamImmutable超出上限即拒绝且不提交(t *testing.T) {
	api := &recordingS3{}
	client := newClientForTest(t, api)
	_, err := client.StreamImmutable(context.Background(), ImmutableStreamInput{
		Key: "gen/text_to_image/" + strings.Repeat("a", 64) + "/step-1.png", ContentType: "image/png",
		TenantSHA256: strings.Repeat("a", 64), MaxBytes: 4,
	}, strings.NewReader("way past the ceiling"))
	if !errors.Is(err, ErrImmutableStreamTooLarge) {
		t.Fatalf("StreamImmutable() error = %v, want ErrImmutableStreamTooLarge", err)
	}
	if api.completeMultipart != nil {
		t.Fatal("over-limit stream still committed an object")
	}
	if len(api.abortedUploads) != 1 {
		t.Fatalf("aborted uploads = %#v, want the partial upload released", api.abortedUploads)
	}
}

func TestClientStreamImmutable分片失败时释放上传且不校验(t *testing.T) {
	api := &recordingS3{uploadPartErr: errors.New("r2 part upload unavailable")}
	client := newClientForTest(t, api)
	_, err := client.StreamImmutable(context.Background(), ImmutableStreamInput{
		Key: "gen/text_to_image/" + strings.Repeat("a", 64) + "/step-1.png", ContentType: "image/png",
		TenantSHA256: strings.Repeat("a", 64), MaxBytes: immutableStreamPartSize + 1,
	}, bytes.NewReader(bytes.Repeat([]byte("a"), immutableStreamPartSize+1)))
	if err == nil {
		t.Fatal("StreamImmutable() error = nil, want the part failure")
	}
	if len(api.abortedUploads) != 1 {
		t.Fatalf("aborted uploads = %#v, want the partial upload released", api.abortedUploads)
	}
	if api.head != nil || api.get != nil {
		t.Fatal("failed stream still ran post-write verification")
	}
}

func TestClientStreamImmutable遇到412视为已存在并释放上传(t *testing.T) {
	content := []byte("concurrently created")
	tenantSHA256 := strings.Repeat("a", 64)
	api := &recordingS3{
		completeMultipartErr: preconditionFailedErr{},
		headOutput:           &s3.HeadObjectOutput{ContentLength: aws.Int64(int64(len(content)))},
		getOutput:            &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(content)), ContentLength: aws.Int64(int64(len(content)))},
	}
	client := newClientForTest(t, api)
	committed, err := client.StreamImmutable(context.Background(), ImmutableStreamInput{
		Key: "gen/text_to_image/" + tenantSHA256 + "/step-1.png", ContentType: "image/png",
		TenantSHA256: tenantSHA256, MaxBytes: 1024,
	}, bytes.NewReader(content))
	if err != nil {
		t.Fatalf("StreamImmutable() error = %v, want the 412 handled as already-exists", err)
	}
	if committed.ContentLength != int64(len(content)) || api.get == nil {
		t.Fatalf("committed/verification = %#v / get=%v", committed, api.get)
	}
	if len(api.abortedUploads) != 1 {
		t.Fatalf("aborted uploads = %#v, want the losing upload released", api.abortedUploads)
	}
}

// preconditionFailedErr is the smallest error that satisfies the SDK's
// IsPreconditionFailed probe without linking a live HTTP response.
type preconditionFailedErr struct{}

func (preconditionFailedErr) Error() string        { return "PreconditionFailed" }
func (preconditionFailedErr) ErrorCode() string    { return "PreconditionFailed" }
func (preconditionFailedErr) ErrorMessage() string { return "PreconditionFailed" }
func (preconditionFailedErr) ErrorFault() smithy.ErrorFault {
	return smithy.FaultClient
}

func newClientForTest(t *testing.T, api s3API, presigners ...presignAPI) *Client {
	t.Helper()
	config := Config{AccountID: "account1", AccessKeyID: "access-1", SecretAccessKey: "secret-1", Region: regionAuto, PublicBucket: "cling-ai", PublicURL: "https://media.example.test"}
	var presigner presignAPI = &recordingPresigner{}
	if len(presigners) == 1 {
		presigner = presigners[0]
	}
	client, err := newClient(config, api, presigner)
	if err != nil {
		t.Fatalf("newClient() error = %v", err)
	}
	return client
}

type recordingS3 struct {
	put              *s3.PutObjectInput
	head             *s3.HeadObjectInput
	get              *s3.GetObjectInput
	deleteMany       []*s3.DeleteObjectsInput
	deleteManyOutput *s3.DeleteObjectsOutput
	copy             *s3.CopyObjectInput
	list             *s3.ListObjectsV2Input
	listOutput       *s3.ListObjectsV2Output
	listErr          error
	headOutput       *s3.HeadObjectOutput
	getOutput        *s3.GetObjectOutput

	// mu guards the multipart fields: the streaming workers upload parts from
	// several goroutines at once, so a bare slice append would race.
	mu                    sync.Mutex
	createMultipart       *s3.CreateMultipartUploadInput
	createMultipartOutput *s3.CreateMultipartUploadOutput
	uploadParts           []*s3.UploadPartInput
	uploadPartErr         error
	completeMultipart     *s3.CompleteMultipartUploadInput
	completeMultipartErr  error
	abortedUploads        []*s3.AbortMultipartUploadInput
}

func (api *recordingS3) PutObject(_ context.Context, input *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	api.put = input
	return &s3.PutObjectOutput{}, nil
}

func (api *recordingS3) GetObject(_ context.Context, input *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	api.get = input
	if api.getOutput != nil {
		return api.getOutput, nil
	}
	return &s3.GetObjectOutput{Body: io.NopCloser(strings.NewReader(""))}, nil
}

func (api *recordingS3) HeadObject(_ context.Context, input *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	api.head = input
	if api.headOutput != nil {
		return api.headOutput, nil
	}
	return &s3.HeadObjectOutput{}, nil
}

func (api *recordingS3) DeleteObject(context.Context, *s3.DeleteObjectInput, ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	return &s3.DeleteObjectOutput{}, nil
}

func (api *recordingS3) DeleteObjects(_ context.Context, input *s3.DeleteObjectsInput, _ ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error) {
	api.deleteMany = append(api.deleteMany, input)
	if api.deleteManyOutput != nil {
		return api.deleteManyOutput, nil
	}
	return &s3.DeleteObjectsOutput{}, nil
}

func (api *recordingS3) CopyObject(_ context.Context, input *s3.CopyObjectInput, _ ...func(*s3.Options)) (*s3.CopyObjectOutput, error) {
	api.copy = input
	return &s3.CopyObjectOutput{}, nil
}

func (api *recordingS3) ListObjectsV2(_ context.Context, input *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	api.list = input
	if api.listErr != nil {
		return nil, api.listErr
	}
	if api.listOutput != nil {
		return api.listOutput, nil
	}
	return &s3.ListObjectsV2Output{}, nil
}

func (*recordingS3) PutBucketCors(context.Context, *s3.PutBucketCorsInput, ...func(*s3.Options)) (*s3.PutBucketCorsOutput, error) {
	return &s3.PutBucketCorsOutput{}, nil
}

func (api *recordingS3) CreateMultipartUpload(_ context.Context, input *s3.CreateMultipartUploadInput, _ ...func(*s3.Options)) (*s3.CreateMultipartUploadOutput, error) {
	api.mu.Lock()
	defer api.mu.Unlock()
	api.createMultipart = input
	if api.createMultipartOutput != nil {
		return api.createMultipartOutput, nil
	}
	return &s3.CreateMultipartUploadOutput{UploadId: aws.String("upload-1")}, nil
}

func (api *recordingS3) UploadPart(_ context.Context, input *s3.UploadPartInput, _ ...func(*s3.Options)) (*s3.UploadPartOutput, error) {
	api.mu.Lock()
	defer api.mu.Unlock()
	api.uploadParts = append(api.uploadParts, input)
	if api.uploadPartErr != nil {
		return nil, api.uploadPartErr
	}
	return &s3.UploadPartOutput{ETag: aws.String("etag-" + strconv.Itoa(len(api.uploadParts)))}, nil
}

func (api *recordingS3) CompleteMultipartUpload(_ context.Context, input *s3.CompleteMultipartUploadInput, _ ...func(*s3.Options)) (*s3.CompleteMultipartUploadOutput, error) {
	api.mu.Lock()
	defer api.mu.Unlock()
	api.completeMultipart = input
	if api.completeMultipartErr != nil {
		return nil, api.completeMultipartErr
	}
	return &s3.CompleteMultipartUploadOutput{}, nil
}

func (api *recordingS3) AbortMultipartUpload(_ context.Context, input *s3.AbortMultipartUploadInput, _ ...func(*s3.Options)) (*s3.AbortMultipartUploadOutput, error) {
	api.mu.Lock()
	defer api.mu.Unlock()
	api.abortedUploads = append(api.abortedUploads, input)
	return &s3.AbortMultipartUploadOutput{}, nil
}

type recordingPresigner struct {
	get        *s3.GetObjectInput
	getExpires time.Duration
}

func (presigner *recordingPresigner) PresignGetObject(_ context.Context, input *s3.GetObjectInput, options ...func(*s3.PresignOptions)) (*awsv4.PresignedHTTPRequest, error) {
	presigner.get = input
	configured := s3.PresignOptions{}
	for _, option := range options {
		option(&configured)
	}
	presigner.getExpires = configured.Expires
	return &awsv4.PresignedHTTPRequest{URL: "https://signed.example.test/object"}, nil
}

func (*recordingPresigner) PresignPutObject(context.Context, *s3.PutObjectInput, ...func(*s3.PresignOptions)) (*awsv4.PresignedHTTPRequest, error) {
	return &awsv4.PresignedHTTPRequest{URL: "https://signed.example.test/object"}, nil
}
