package r2

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsv4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

const (
	immutableMediaCacheControl = "public, max-age=31536000, immutable"
	maxDeleteBatch             = 1000
	maxPresignExpiry           = 7 * 24 * time.Hour
	// immutableStreamPartSize is the streaming part size. S3 requires every part
	// except the last to be at least 5 MiB, so 8 MiB both satisfies the floor and
	// keeps a bounded in-flight window in memory instead of staging the whole
	// object on the worker's local disk.
	immutableStreamPartSize = 8 << 20
	// immutableStreamPartQueueSize is the frozen multipart concurrency for the
	// public bucket (§6.1 "queueSize = 4"). Parts upload in parallel while the
	// transient footprint stays bounded by (queueSize + 1) parts.
	immutableStreamPartQueueSize = 4
	// immutableStreamCleanupTimeout bounds the detached AbortMultipartUpload that
	// runs after the streaming context has already failed.
	immutableStreamCleanupTimeout = 30 * time.Second
)

var (
	ErrClientUnavailable     = errors.New("r2 client is unavailable")
	ErrInvalidObjectRequest  = errors.New("r2 object request is invalid")
	ErrBatchDeleteIncomplete = errors.New("r2 batch delete is incomplete")
	// ErrImmutableObjectVerification means R2 did not prove that the object
	// matching the caller's immutable identity exists byte-for-byte. It must be
	// retried or sent for operator attention; callers must not publish it.
	ErrImmutableObjectVerification = errors.New("r2 immutable object verification failed")
	// ErrImmutableStreamTooLarge means a streamed body crossed the caller's
	// declared ceiling. The caller must fail the work item closed instead of
	// retrying it, because the same provider response would exceed it again.
	ErrImmutableStreamTooLarge = errors.New("r2 immutable stream exceeds the declared maximum")
)

// Client is a narrow S3-compatible R2 adapter. The business and data layers
// depend on its R2-native inputs rather than AWS SDK request structures, so
// the SDK cannot leak into domain contracts.
type Client struct {
	config    Config
	api       s3API
	presigner presignAPI
	closeIdle func()
}

type s3API interface {
	PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	HeadObject(context.Context, *s3.HeadObjectInput, ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	DeleteObject(context.Context, *s3.DeleteObjectInput, ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
	DeleteObjects(context.Context, *s3.DeleteObjectsInput, ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error)
	CopyObject(context.Context, *s3.CopyObjectInput, ...func(*s3.Options)) (*s3.CopyObjectOutput, error)
	ListObjectsV2(context.Context, *s3.ListObjectsV2Input, ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	PutBucketCors(context.Context, *s3.PutBucketCorsInput, ...func(*s3.Options)) (*s3.PutBucketCorsOutput, error)
	CreateMultipartUpload(context.Context, *s3.CreateMultipartUploadInput, ...func(*s3.Options)) (*s3.CreateMultipartUploadOutput, error)
	UploadPart(context.Context, *s3.UploadPartInput, ...func(*s3.Options)) (*s3.UploadPartOutput, error)
	CompleteMultipartUpload(context.Context, *s3.CompleteMultipartUploadInput, ...func(*s3.Options)) (*s3.CompleteMultipartUploadOutput, error)
	AbortMultipartUpload(context.Context, *s3.AbortMultipartUploadInput, ...func(*s3.Options)) (*s3.AbortMultipartUploadOutput, error)
}

type presignAPI interface {
	PresignGetObject(context.Context, *s3.GetObjectInput, ...func(*s3.PresignOptions)) (*awsv4.PresignedHTTPRequest, error)
	PresignPutObject(context.Context, *s3.PutObjectInput, ...func(*s3.PresignOptions)) (*awsv4.PresignedHTTPRequest, error)
}

// NewClient constructs an authenticated R2 client but does not make any
// network request. Its endpoint is always derived from R2_ACCOUNT_ID; it
// intentionally does not honor R2_ENDPOINT, which is reserved for legacy GPU
// onboarding tooling rather than application traffic.
func NewClient(config Config) (*Client, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	awsConfig := aws.Config{
		Region:      regionAuto,
		Credentials: credentials.NewStaticCredentialsProvider(config.AccessKeyID, config.SecretAccessKey, ""),
		HTTPClient:  &http.Client{Transport: transport},
	}
	api := s3.NewFromConfig(awsConfig, func(options *s3.Options) {
		// Match the legacy SDK client: use the account endpoint and let the S3
		// resolver keep its default addressing behavior. ForcePathStyle would
		// change the frozen request topology without a migration decision.
		options.BaseEndpoint = aws.String(config.Endpoint())
	})
	client, err := newClient(config, api, s3.NewPresignClient(api))
	if err != nil {
		return nil, err
	}
	client.closeIdle = transport.CloseIdleConnections
	return client, nil
}

func newClient(config Config, api s3API, presigner presignAPI) (*Client, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if api == nil || presigner == nil {
		return nil, ErrClientUnavailable
	}
	return &Client{config: config, api: api, presigner: presigner}, nil
}

// CloseIdleConnections releases SDK transport idle connections. It does not
// cancel any in-flight upload/download.
func (client *Client) CloseIdleConnections() {
	if client != nil && client.closeIdle != nil {
		client.closeIdle()
	}
}

type PutInput struct {
	Bucket             string
	Key                string
	Body               io.Reader
	ContentType        string
	ContentLength      *int64
	CacheControl       string
	ContentDisposition string
	Metadata           map[string]string
}

type ObjectInfo struct {
	Exists        bool
	ContentType   string
	ContentLength int64
	ETag          string
	LastModified  time.Time
}

// Put writes one object and returns only transport metadata. Callers choose a
// stable URL through Config.PublicObjectURL or a proxy-presigned URL; an S3
// write response is never treated as a public asset reference by itself.
func (client *Client) Put(ctx context.Context, input PutInput) (ObjectInfo, error) {
	if !client.ready() || input.Body == nil || !validObjectKey(input.Key) {
		return ObjectInfo{}, ErrInvalidObjectRequest
	}
	output, err := client.api.PutObject(ctx, &s3.PutObjectInput{
		Bucket:             aws.String(client.bucket(input.Bucket)),
		Key:                aws.String(input.Key),
		Body:               input.Body,
		ContentType:        optional(input.ContentType),
		ContentLength:      input.ContentLength,
		CacheControl:       optional(cacheControl(input.ContentType, input.CacheControl)),
		ContentDisposition: optional(input.ContentDisposition),
		Metadata:           cloneMetadata(input.Metadata),
	})
	if err != nil {
		return ObjectInfo{}, fmt.Errorf("put r2 object: %w", err)
	}
	return ObjectInfo{Exists: true, ETag: aws.ToString(output.ETag)}, nil
}

// ImmutablePutInput binds a generated result to the content and tenant
// digests that form its public R2 key. The caller has already streamed and
// hashed the untrusted provider response; this boundary writes it with R2's
// conditional checksum contract and independently reads it back before it
// can become a user-visible asset.
type ImmutablePutInput struct {
	Bucket        string
	Key           string
	Body          io.Reader
	ContentType   string
	ContentLength int64
	ContentSHA256 string
	TenantSHA256  string
}

// PutImmutable implements the frozen generation-service §6.3 contract:
// conditionally create, use an encoded SHA-256 checksum, then prove both size
// and bytes through an independent HEAD + full GET. A 412 means a concurrent
// worker may already have created the same immutable key, so it follows the
// exact same verification path instead of overwriting or trusting it.
func (client *Client) PutImmutable(ctx context.Context, input ImmutablePutInput) (ObjectInfo, error) {
	if !client.ready() || input.Body == nil || !validImmutablePutInput(input) {
		return ObjectInfo{}, ErrInvalidObjectRequest
	}
	contentDigest, _ := hex.DecodeString(input.ContentSHA256)
	output, err := client.api.PutObject(ctx, &s3.PutObjectInput{
		Bucket:         aws.String(client.bucket(input.Bucket)),
		Key:            aws.String(input.Key),
		Body:           input.Body,
		ContentType:    aws.String(input.ContentType),
		ContentLength:  aws.Int64(input.ContentLength),
		CacheControl:   aws.String(cacheControl(input.ContentType, "")),
		IfNoneMatch:    aws.String("*"),
		ChecksumSHA256: aws.String(base64.StdEncoding.EncodeToString(contentDigest)),
		Metadata:       map[string]string{"content-sha256": input.ContentSHA256, "tenant-sha256": input.TenantSHA256},
	})
	if err != nil && !IsPreconditionFailed(err) {
		return ObjectInfo{}, fmt.Errorf("put immutable r2 object: %w", err)
	}
	if err := client.verifyImmutableObject(ctx, input.Bucket, input.Key, input.ContentLength, input.ContentSHA256); err != nil {
		return ObjectInfo{}, err
	}
	info := ObjectInfo{Exists: true}
	if output != nil {
		info.ETag = aws.ToString(output.ETag)
	}
	return info, nil
}

func (client *Client) verifyImmutableObject(ctx context.Context, bucket, key string, contentLength int64, contentSHA256 string) error {
	head, err := client.Head(ctx, bucket, key)
	if err != nil {
		return fmt.Errorf("%w: head object: %v", ErrImmutableObjectVerification, err)
	}
	if !head.Exists || head.ContentLength != contentLength {
		return ErrImmutableObjectVerification
	}
	object, err := client.Get(ctx, GetInput{Bucket: bucket, Key: key})
	if err != nil {
		return fmt.Errorf("%w: get object: %v", ErrImmutableObjectVerification, err)
	}
	if object == nil || !object.Exists || object.Body == nil {
		return ErrImmutableObjectVerification
	}
	defer func() { _ = object.Body.Close() }()

	// Read one byte past the declared size. This prevents a malformed or
	// inconsistent R2 response from being accepted merely because its first N
	// bytes hash correctly.
	limited := &io.LimitedReader{R: object.Body, N: contentLength + 1}
	digest := sha256.New()
	read, readErr := io.Copy(digest, limited)
	if readErr != nil || read != contentLength || limited.N == 0 {
		return ErrImmutableObjectVerification
	}
	if got := hex.EncodeToString(digest.Sum(nil)); got != contentSHA256 {
		return ErrImmutableObjectVerification
	}
	return nil
}

// ImmutableStreamInput describes one immutable object that is streamed straight
// from the caller's source reader into R2.
//
// Key is required up front, which is the whole point of this entry point: a
// multipart upload freezes its destination key at CreateMultipartUpload, so a
// caller must not have to materialize the body locally merely to learn a
// content-derived name before the first byte leaves the socket.
type ImmutableStreamInput struct {
	Bucket       string
	Key          string
	ContentType  string
	TenantSHA256 string
	// MaxBytes is a fail-closed ceiling for the streamed body. It is enforced
	// here as well as by the caller so the client cannot be misused into
	// buffering an unbounded provider response.
	MaxBytes int64
}

// ImmutableStreamedObject is the committed identity of one streamed object.
type ImmutableStreamedObject struct {
	Key           string
	ContentSHA256 string
	ContentLength int64
}

// ImmutableStream streams one body into R2 with multipart upload instead of
// staging it on the worker's local disk.
//
// It carries the same guarantees as PutImmutable, moved to the stage where
// multipart can express them:
//
//   - The conditional create moves to CompleteMultipartUpload, whose
//     IfNoneMatch is the only stage that can atomically reveal the object. A
//     412 therefore means a concurrent attempt already created the same key and
//     is verified instead of overwritten, exactly like the single-PUT path.
//   - Every byte is hashed while it streams, and Commit still proves the
//     published object through an independent HEAD plus a full read-back of the
//     stored bytes.
//   - "content-sha256" object metadata is intentionally absent. Multipart
//     metadata is frozen at CreateMultipartUpload, when the content hash is not
//     knowable yet; the hash is instead recorded durably by the caller on the
//     published asset row.
//
// The stream is bound to the context passed to BeginImmutableStream. Callers
// must call Abort after any failure before Commit; Abort is a no-op once the
// upload has been committed or already aborted.
type ImmutableStream struct {
	client   *Client
	ctx      context.Context
	cancel   context.CancelFunc
	bucket   string
	key      string
	uploadID string
	maxBytes int64
	buffer   []byte
	digest   hash.Hash
	written  int64
	nextPart int32
	closed   bool
	drained  bool

	// queue and workers implement the frozen multipart write parameters: at
	// most immutableStreamPartQueueSize parts are in flight, so the transient
	// footprint is bounded by (queueSize + 1) parts instead of the object size.
	queue   chan immutableStreamPart
	workers sync.WaitGroup
	results sync.Mutex
	parts   []types.CompletedPart
	failure error
}

type immutableStreamPart struct {
	number int32
	body   []byte
}

func (client *Client) BeginImmutableStream(ctx context.Context, input ImmutableStreamInput) (*ImmutableStream, error) {
	if !client.ready() || ctx == nil || !validImmutableStreamInput(input) {
		return nil, ErrInvalidObjectRequest
	}
	bucket := client.bucket(input.Bucket)
	output, err := client.api.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket:       aws.String(bucket),
		Key:          aws.String(input.Key),
		ContentType:  aws.String(input.ContentType),
		CacheControl: aws.String(cacheControl(input.ContentType, "")),
		Metadata:     map[string]string{"tenant-sha256": input.TenantSHA256},
	})
	if err != nil {
		return nil, fmt.Errorf("create multipart r2 object: %w", err)
	}
	if output == nil || aws.ToString(output.UploadId) == "" {
		return nil, ErrInvalidObjectRequest
	}
	streamCtx, cancel := context.WithCancel(ctx)
	stream := &ImmutableStream{
		client: client, ctx: streamCtx, cancel: cancel, bucket: bucket, key: input.Key,
		uploadID: aws.ToString(output.UploadId), maxBytes: input.MaxBytes,
		buffer: make([]byte, 0, immutableStreamPartSize), digest: sha256.New(),
		queue: make(chan immutableStreamPart, immutableStreamPartQueueSize),
	}
	stream.workers.Add(immutableStreamPartQueueSize)
	for index := 0; index < immutableStreamPartQueueSize; index++ {
		go stream.uploadPartWorker()
	}
	return stream, nil
}

// uploadPartWorker drains the bounded part queue. A failed part cancels the
// stream so no further part is attempted and blocked writers stop waiting.
func (stream *ImmutableStream) uploadPartWorker() {
	defer stream.workers.Done()
	for part := range stream.queue {
		output, err := stream.client.api.UploadPart(stream.ctx, &s3.UploadPartInput{
			Bucket:        aws.String(stream.bucket),
			Key:           aws.String(stream.key),
			UploadId:      aws.String(stream.uploadID),
			PartNumber:    aws.Int32(part.number),
			Body:          bytes.NewReader(part.body),
			ContentLength: aws.Int64(int64(len(part.body))),
		})
		if err != nil {
			stream.fail(fmt.Errorf("upload r2 immutable part: %w", err))
			return
		}
		if output == nil || aws.ToString(output.ETag) == "" {
			stream.fail(ErrInvalidObjectRequest)
			return
		}
		stream.results.Lock()
		stream.parts = append(stream.parts, types.CompletedPart{ETag: output.ETag, PartNumber: aws.Int32(part.number)})
		stream.results.Unlock()
	}
}

func (stream *ImmutableStream) fail(cause error) {
	stream.results.Lock()
	if stream.failure == nil {
		stream.failure = cause
	}
	stream.results.Unlock()
	stream.cancel()
}

func (stream *ImmutableStream) failureError() error {
	stream.results.Lock()
	defer stream.results.Unlock()
	return stream.failure
}

// completedParts returns the uploaded parts in part-number order, which is the
// order CompleteMultipartUpload requires.
func (stream *ImmutableStream) completedParts() []types.CompletedPart {
	stream.results.Lock()
	defer stream.results.Unlock()
	parts := make([]types.CompletedPart, len(stream.parts))
	copy(parts, stream.parts)
	sort.Slice(parts, func(left, right int) bool {
		return aws.ToInt32(parts[left].PartNumber) < aws.ToInt32(parts[right].PartNumber)
	})
	return parts
}

// drain stops accepting parts and waits for the in-flight ones. It is
// idempotent so Commit and Abort can both call it.
func (stream *ImmutableStream) drain() error {
	if !stream.drained {
		stream.drained = true
		close(stream.queue)
	}
	stream.workers.Wait()
	return stream.failureError()
}

// Write satisfies io.Writer. It never holds more than one part in the writer
// plus the bounded in-flight queue, and never accepts more than MaxBytes, so a
// hostile or misbehaving source cannot make the worker hold the whole response.
func (stream *ImmutableStream) Write(p []byte) (int, error) {
	if stream == nil || stream.closed || stream.uploadID == "" {
		return 0, ErrInvalidObjectRequest
	}
	if err := stream.failureError(); err != nil {
		return 0, err
	}
	if stream.maxBytes > 0 && stream.written+int64(len(p)) > stream.maxBytes {
		return 0, ErrImmutableStreamTooLarge
	}
	if _, err := stream.digest.Write(p); err != nil {
		return 0, err
	}
	stream.written += int64(len(p))
	consumed := 0
	for consumed < len(p) {
		if len(stream.buffer) == immutableStreamPartSize {
			if err := stream.dispatchPart(); err != nil {
				return consumed, err
			}
		}
		space := immutableStreamPartSize - len(stream.buffer)
		take := len(p) - consumed
		if take > space {
			take = space
		}
		stream.buffer = append(stream.buffer, p[consumed:consumed+take]...)
		consumed += take
	}
	return consumed, nil
}

// dispatchPart hands one full part to the worker queue and takes a fresh buffer,
// so a part in flight is never mutated by the writer. A full queue blocks the
// reader, which is the intended backpressure.
func (stream *ImmutableStream) dispatchPart() error {
	if len(stream.buffer) == 0 {
		return nil
	}
	if err := stream.failureError(); err != nil {
		return err
	}
	stream.nextPart++
	part := immutableStreamPart{number: stream.nextPart, body: stream.buffer}
	stream.buffer = make([]byte, 0, immutableStreamPartSize)
	select {
	case stream.queue <- part:
		return nil
	case <-stream.ctx.Done():
		if err := stream.failureError(); err != nil {
			return err
		}
		return stream.ctx.Err()
	}
}

// Commit finishes the multipart upload and proves the published object the same
// way PutImmutable does. A failed Commit leaves the stream open so the caller
// can Abort it; a 412 closes it after releasing the now-useless upload.
func (stream *ImmutableStream) Commit(ctx context.Context) (ImmutableStreamedObject, error) {
	if stream == nil || stream.closed || stream.uploadID == "" || ctx == nil || stream.written < 1 {
		return ImmutableStreamedObject{}, ErrInvalidObjectRequest
	}
	if err := stream.dispatchPart(); err != nil {
		return ImmutableStreamedObject{}, err
	}
	if err := stream.drain(); err != nil {
		return ImmutableStreamedObject{}, err
	}
	defer stream.cancel()
	contentSHA256 := hex.EncodeToString(stream.digest.Sum(nil))
	_, err := stream.client.api.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:          aws.String(stream.bucket),
		Key:             aws.String(stream.key),
		UploadId:        aws.String(stream.uploadID),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: stream.completedParts()},
		IfNoneMatch:     aws.String("*"),
	})
	precondition := IsPreconditionFailed(err)
	if err != nil && !precondition {
		return ImmutableStreamedObject{}, fmt.Errorf("complete multipart r2 object: %w", err)
	}
	if precondition {
		// The object already exists, so the upload id is dead weight. Release it
		// before verifying the winner; a failed release must not mask the
		// verification result, and it never changes what gets published. Abort
		// must run before this stream is marked closed, or it would short-circuit
		// and leak the losing upload.
		_ = stream.Abort(ctx)
	}
	stream.closed = true
	if err := stream.client.verifyImmutableObject(ctx, stream.bucket, stream.key, stream.written, contentSHA256); err != nil {
		return ImmutableStreamedObject{}, err
	}
	return ImmutableStreamedObject{Key: stream.key, ContentSHA256: contentSHA256, ContentLength: stream.written}, nil
}

// Abort releases an uncommitted multipart upload. It is idempotent and returns
// nil for a stream that was already committed or aborted.
func (stream *ImmutableStream) Abort(ctx context.Context) error {
	if stream == nil || stream.closed || stream.uploadID == "" {
		return nil
	}
	stream.closed = true
	// Stop the part workers and cancel in-flight uploads before aborting the
	// upload itself, so no UploadPart races the AbortMultipartUpload that is
	// about to invalidate this upload id.
	stream.cancel()
	_ = stream.drain()
	if ctx == nil {
		ctx = context.Background()
	}
	if _, err := stream.client.api.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket: aws.String(stream.bucket), Key: aws.String(stream.key), UploadId: aws.String(stream.uploadID),
	}); err != nil {
		return fmt.Errorf("abort multipart r2 object: %w", err)
	}
	return nil
}

// StreamImmutable copies source into one immutable R2 object and proves the
// result. It owns the multipart lifecycle so a caller cannot forget to release
// a partially uploaded object: every failure before commit aborts the upload
// with a detached context, because in the common case the caller's context is
// itself the thing that failed.
func (client *Client) StreamImmutable(ctx context.Context, input ImmutableStreamInput, source io.Reader) (ImmutableStreamedObject, error) {
	if source == nil {
		return ImmutableStreamedObject{}, ErrInvalidObjectRequest
	}
	stream, err := client.BeginImmutableStream(ctx, input)
	if err != nil {
		return ImmutableStreamedObject{}, err
	}
	if _, err := io.Copy(stream, source); err != nil {
		abortDetached(stream, ctx)
		return ImmutableStreamedObject{}, err
	}
	committed, err := stream.Commit(ctx)
	if err != nil {
		abortDetached(stream, ctx)
		return ImmutableStreamedObject{}, err
	}
	return committed, nil
}

// abortDetached releases a partial upload even though ctx has usually already
// failed; the cleanup must not inherit that failure.
func abortDetached(stream *ImmutableStream, ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), immutableStreamCleanupTimeout)
	defer cancel()
	_ = stream.Abort(cleanupCtx)
}

type GetInput struct {
	Bucket string
	Key    string
	Range  string
}

type Object struct {
	ObjectInfo
	Body         io.ReadCloser
	ContentRange string
}

// Get returns Exists=false for a confirmed 404, matching the old provider's
// object API. Other transport failures remain errors; callers must not turn
// them into a missing object.
func (client *Client) Get(ctx context.Context, input GetInput) (*Object, error) {
	if !client.ready() || !validObjectKey(input.Key) {
		return nil, ErrInvalidObjectRequest
	}
	output, err := client.api.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(client.bucket(input.Bucket)), Key: aws.String(input.Key), Range: optional(input.Range),
	})
	if err != nil {
		if IsNotFound(err) {
			return &Object{}, nil
		}
		return nil, fmt.Errorf("get r2 object: %w", err)
	}
	return &Object{
		ObjectInfo: ObjectInfo{Exists: true, ContentType: aws.ToString(output.ContentType), ContentLength: aws.ToInt64(output.ContentLength), ETag: aws.ToString(output.ETag), LastModified: aws.ToTime(output.LastModified)},
		Body:       output.Body, ContentRange: aws.ToString(output.ContentRange),
	}, nil
}

// Head returns Exists=false only for a confirmed 404.
func (client *Client) Head(ctx context.Context, bucket, key string) (ObjectInfo, error) {
	if !client.ready() || !validObjectKey(key) {
		return ObjectInfo{}, ErrInvalidObjectRequest
	}
	output, err := client.api.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(client.bucket(bucket)), Key: aws.String(key)})
	if err != nil {
		if IsNotFound(err) {
			return ObjectInfo{}, nil
		}
		return ObjectInfo{}, fmt.Errorf("head r2 object: %w", err)
	}
	return ObjectInfo{Exists: true, ContentType: aws.ToString(output.ContentType), ContentLength: aws.ToInt64(output.ContentLength), ETag: aws.ToString(output.ETag), LastModified: aws.ToTime(output.LastModified)}, nil
}

func (client *Client) Delete(ctx context.Context, bucket, key string) error {
	if !client.ready() || !validObjectKey(key) {
		return ErrInvalidObjectRequest
	}
	if _, err := client.api.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(client.bucket(bucket)), Key: aws.String(key)}); err != nil {
		return fmt.Errorf("delete r2 object: %w", err)
	}
	return nil
}

// DeleteMany performs the old provider's dedupe + maximum 1000-object batch
// protocol. A partial S3/R2 response is a hard error; pretending it succeeded
// would leave orphaned private media without an operator-visible retry.
func (client *Client) DeleteMany(ctx context.Context, bucket string, keys []string) (int, error) {
	if !client.ready() {
		return 0, ErrClientUnavailable
	}
	unique := uniqueKeys(keys)
	if len(unique) == 0 {
		return 0, nil
	}
	for _, key := range unique {
		if !validObjectKey(key) {
			return 0, ErrInvalidObjectRequest
		}
	}
	for from := 0; from < len(unique); from += maxDeleteBatch {
		to := from + maxDeleteBatch
		if to > len(unique) {
			to = len(unique)
		}
		objects := make([]types.ObjectIdentifier, 0, to-from)
		for _, key := range unique[from:to] {
			objects = append(objects, types.ObjectIdentifier{Key: aws.String(key)})
		}
		output, err := client.api.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(client.bucket(bucket)), Delete: &types.Delete{Objects: objects, Quiet: aws.Bool(true)},
		})
		if err != nil {
			return 0, fmt.Errorf("delete r2 objects: %w", err)
		}
		if len(output.Errors) > 0 {
			return 0, ErrBatchDeleteIncomplete
		}
	}
	return len(unique), nil
}

type CopyInput struct {
	SourceBucket      string
	SourceKey         string
	DestinationBucket string
	DestinationKey    string
	ContentType       string
	CacheControl      string
}

// Copy executes a same-account server-side R2 copy and replaces source
// metadata with the explicitly reviewed result metadata.
func (client *Client) Copy(ctx context.Context, input CopyInput) (ObjectInfo, error) {
	if !client.ready() || !validBucket(input.SourceBucket) || !validObjectKey(input.SourceKey) || !validObjectKey(input.DestinationKey) {
		return ObjectInfo{}, ErrInvalidObjectRequest
	}
	destination := client.bucket(input.DestinationBucket)
	output, err := client.api.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:            aws.String(destination),
		Key:               aws.String(input.DestinationKey),
		CopySource:        aws.String(copySource(input.SourceBucket, input.SourceKey)),
		ContentType:       optional(input.ContentType),
		CacheControl:      optional(cacheControl(input.ContentType, input.CacheControl)),
		MetadataDirective: types.MetadataDirectiveReplace,
	})
	if err != nil {
		return ObjectInfo{}, fmt.Errorf("copy r2 object: %w", err)
	}
	info := ObjectInfo{Exists: true}
	if output.CopyObjectResult != nil {
		info.ETag = aws.ToString(output.CopyObjectResult.ETag)
		info.LastModified = aws.ToTime(output.CopyObjectResult.LastModified)
	}
	return info, nil
}

type PresignGetInput struct {
	Bucket                     string
	Key                        string
	Expires                    time.Duration
	ResponseContentDisposition string
}

// PublicBucketName exposes the configured public bucket to HTTP adapters
// without letting them derive or substitute a bucket name.
func (client *Client) PublicBucketName() string {
	if client == nil {
		return ""
	}
	return client.config.PublicBucket
}

// PrivateBucketName exposes only the explicitly configured private bucket.
func (client *Client) PrivateBucketName() string {
	if client == nil {
		return ""
	}
	return client.config.PrivateBucketName()
}

func (client *Client) PresignGet(ctx context.Context, input PresignGetInput) (string, error) {
	if !client.ready() || !validObjectKey(input.Key) || !validPresignExpiry(input.Expires) {
		return "", ErrInvalidObjectRequest
	}
	output, err := client.presigner.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(client.bucket(input.Bucket)), Key: aws.String(input.Key), ResponseContentDisposition: optional(input.ResponseContentDisposition),
	}, func(options *s3.PresignOptions) { options.Expires = input.Expires })
	if err != nil {
		return "", fmt.Errorf("presign r2 get: %w", err)
	}
	return output.URL, nil
}

type PresignPutInput struct {
	Bucket       string
	Key          string
	ContentType  string
	CacheControl string
	Expires      time.Duration
}

func (client *Client) PresignPut(ctx context.Context, input PresignPutInput) (string, error) {
	if !client.ready() || !validObjectKey(input.Key) || !validPresignExpiry(input.Expires) {
		return "", ErrInvalidObjectRequest
	}
	output, err := client.presigner.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(client.bucket(input.Bucket)), Key: aws.String(input.Key), ContentType: optional(input.ContentType), CacheControl: optional(cacheControl(input.ContentType, input.CacheControl)),
	}, func(options *s3.PresignOptions) { options.Expires = input.Expires })
	if err != nil {
		return "", fmt.Errorf("presign r2 put: %w", err)
	}
	return output.URL, nil
}

type ListInput struct {
	Bucket            string
	Prefix            string
	MaxKeys           int32
	ContinuationToken string
}

type ListedObject struct {
	Key          string
	Size         int64
	ETag         string
	LastModified time.Time
}

type ListOutput struct {
	Contents              []ListedObject
	IsTruncated           bool
	NextContinuationToken string
}

func (client *Client) List(ctx context.Context, input ListInput) (ListOutput, error) {
	if !client.ready() || input.MaxKeys < 0 || input.MaxKeys > maxDeleteBatch || strings.TrimSpace(input.Prefix) != input.Prefix {
		return ListOutput{}, ErrInvalidObjectRequest
	}
	maxKeys := input.MaxKeys
	if maxKeys == 0 {
		maxKeys = maxDeleteBatch
	}
	output, err := client.api.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(client.bucket(input.Bucket)), Prefix: optional(input.Prefix), MaxKeys: aws.Int32(maxKeys), ContinuationToken: optional(input.ContinuationToken),
	})
	if err != nil {
		return ListOutput{}, fmt.Errorf("list r2 objects: %w", err)
	}
	result := ListOutput{IsTruncated: aws.ToBool(output.IsTruncated), NextContinuationToken: aws.ToString(output.NextContinuationToken), Contents: make([]ListedObject, 0, len(output.Contents))}
	for _, item := range output.Contents {
		result.Contents = append(result.Contents, ListedObject{Key: aws.ToString(item.Key), Size: aws.ToInt64(item.Size), ETag: aws.ToString(item.ETag), LastModified: aws.ToTime(item.LastModified)})
	}
	return result, nil
}

type CORSRule struct {
	AllowedHeaders []string
	AllowedMethods []string
	AllowedOrigins []string
	ExposeHeaders  []string
	MaxAgeSeconds  int32
}

func (client *Client) PutBucketCORS(ctx context.Context, bucket string, rules []CORSRule) error {
	if !client.ready() || len(rules) == 0 {
		return ErrInvalidObjectRequest
	}
	mapped := make([]types.CORSRule, 0, len(rules))
	for _, rule := range rules {
		if len(rule.AllowedMethods) == 0 || len(rule.AllowedOrigins) == 0 || rule.MaxAgeSeconds < 0 {
			return ErrInvalidObjectRequest
		}
		mapped = append(mapped, types.CORSRule{AllowedHeaders: append([]string(nil), rule.AllowedHeaders...), AllowedMethods: append([]string(nil), rule.AllowedMethods...), AllowedOrigins: append([]string(nil), rule.AllowedOrigins...), ExposeHeaders: append([]string(nil), rule.ExposeHeaders...), MaxAgeSeconds: aws.Int32(rule.MaxAgeSeconds)})
	}
	if _, err := client.api.PutBucketCors(ctx, &s3.PutBucketCorsInput{Bucket: aws.String(client.bucket(bucket)), CORSConfiguration: &types.CORSConfiguration{CORSRules: mapped}}); err != nil {
		return fmt.Errorf("put r2 bucket cors: %w", err)
	}
	return nil
}

// IsNotFound recognizes only explicit S3 API/HTTP not-found responses. DNS,
// credential and transport errors remain errors so a materializer cannot mark
// a result missing merely because R2 was temporarily unavailable.
func IsNotFound(err error) bool {
	var response *smithyhttp.ResponseError
	if errors.As(err, &response) && response.HTTPStatusCode() == 404 {
		return true
	}
	var apiError smithy.APIError
	return errors.As(err, &apiError) && apiError.ErrorCode() == "NotFound"
}

// IsPreconditionFailed recognizes only explicit S3/R2 conditional-create
// conflicts. It deliberately does not classify generic 4xx failures as an
// existing immutable object, because a bad credential or malformed request
// must never turn into a published result.
func IsPreconditionFailed(err error) bool {
	var response *smithyhttp.ResponseError
	if errors.As(err, &response) && response.HTTPStatusCode() == http.StatusPreconditionFailed {
		return true
	}
	var apiError smithy.APIError
	return errors.As(err, &apiError) && apiError.ErrorCode() == "PreconditionFailed"
}

func (client *Client) ready() bool {
	return client != nil && client.api != nil && client.presigner != nil
}

func (client *Client) bucket(value string) string {
	if value == "" {
		return client.config.PublicBucket
	}
	return value
}

func cacheControl(contentType, configured string) string {
	if configured != "" {
		return configured
	}
	if strings.HasPrefix(contentType, "image/") || strings.HasPrefix(contentType, "video/") {
		return immutableMediaCacheControl
	}
	return ""
}

func optional(value string) *string {
	if value == "" {
		return nil
	}
	return aws.String(value)
}

func cloneMetadata(input map[string]string) map[string]string {
	if len(input) == 0 {
		return nil
	}
	output := make(map[string]string, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}

func uniqueKeys(keys []string) []string {
	seen := make(map[string]struct{}, len(keys))
	unique := make([]string, 0, len(keys))
	for _, key := range keys {
		if key == "" {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		unique = append(unique, key)
	}
	return unique
}

func copySource(bucket, key string) string {
	return bucket + "/" + (&url.URL{Path: key}).EscapedPath()
}

func validPresignExpiry(value time.Duration) bool { return value > 0 && value <= maxPresignExpiry }

func validImmutablePutInput(input ImmutablePutInput) bool {
	if input.ContentLength < 1 || !validSHA256Hex(input.ContentSHA256) || !validSHA256Hex(input.TenantSHA256) ||
		!validImmutableObjectKey(input.Key) {
		return false
	}
	segments, extension, ok := immutableKeySegmentsAndExtension(input.Key)
	if !ok {
		return false
	}
	fileName := segments[len(segments)-1]
	// The single-PUT contract names the object after its own content, so the
	// filename stem must be the declared content hash.
	if segments[len(segments)-2] != input.TenantSHA256 || fileName[:len(fileName)-len(extension)-1] != input.ContentSHA256 {
		return false
	}
	return immutableContentType(extension, input.ContentType)
}

// validImmutableStreamInput keeps the tenant partition and the
// extension/content-type agreement, but deliberately does not require the
// filename to be the content hash: a streamed upload cannot know that hash
// before it has chosen its key.
func validImmutableStreamInput(input ImmutableStreamInput) bool {
	if input.MaxBytes < 1 || input.ContentType == "" || !validSHA256Hex(input.TenantSHA256) ||
		!validImmutableObjectKey(input.Key) {
		return false
	}
	segments, extension, ok := immutableKeySegmentsAndExtension(input.Key)
	if !ok {
		return false
	}
	if segments[len(segments)-2] != input.TenantSHA256 {
		return false
	}
	return immutableContentType(extension, input.ContentType)
}

// immutableKeySegmentsAndExtension applies the path shape shared by every
// immutable key: at least three non-empty segments, none of them a relative
// path segment, and a real file extension.
func immutableKeySegmentsAndExtension(key string) ([]string, string, bool) {
	segments := strings.Split(key, "/")
	if len(segments) < 3 {
		return nil, "", false
	}
	for _, segment := range segments {
		if segment == "" || segment == "." || segment == ".." {
			return nil, "", false
		}
	}
	fileName := segments[len(segments)-1]
	extensionIndex := strings.LastIndexByte(fileName, '.')
	if extensionIndex < 1 || extensionIndex == len(fileName)-1 {
		return nil, "", false
	}
	return segments, strings.ToLower(fileName[extensionIndex+1:]), true
}

func validImmutableObjectKey(key string) bool {
	if len(key) == 0 || len(key) > 1024 || !isImmutableKeyAlnum(key[0]) || !isImmutableKeyAlnum(key[len(key)-1]) {
		return false
	}
	for index := 1; index < len(key)-1; index++ {
		char := key[index]
		if !isImmutableKeyAlnum(char) && char != '.' && char != '_' && char != '/' && char != '-' {
			return false
		}
	}
	return true
}

func isImmutableKeyAlnum(char byte) bool {
	return char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9'
}

func validSHA256Hex(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func immutableContentType(extension, contentType string) bool {
	switch extension {
	case "jpg":
		return contentType == "image/jpeg"
	case "png":
		return contentType == "image/png"
	case "webp":
		return contentType == "image/webp"
	case "mp4":
		return contentType == "video/mp4" || contentType == "video/quicktime"
	case "webm":
		return contentType == "video/webm"
	default:
		return false
	}
}
