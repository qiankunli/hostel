// Copyright 2026 Li Qiankun
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// s3OpTimeout bounds a single object operation. Generous: chunk transfers can
// ride slow links; lifecycle callers pass Background contexts.
const s3OpTimeout = 5 * time.Minute

// generationMetaKey is the S3 user-metadata key carrying the bed generation
// (served back by HEAD, so Stat never downloads the snapshot). The SDK strips
// the x-amz-meta- prefix and lowercases keys on read.
const generationMetaKey = "generation"

// newS3Client builds the shared S3-compatible client.
func newS3Client(ctx context.Context, cfg Config) (*s3.Client, error) {
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: aws config: %w", err)
	}
	return s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = &cfg.Endpoint
		}
		// TOS only supports virtual-hosted buckets, while some MinIO/Ceph
		// deployments require path-style. Keep the interoperable default and
		// let deployments opt into path-style explicitly.
		o.UsePathStyle = cfg.PathStyle
	}), nil
}

// objAPI is the minimal object-storage surface the cas backend needs. It
// exists so the cas persist/restore/GC logic — the part with actual room for
// bugs — is unit-testable against an in-memory fake; s3obj is a thin adapter
// with nothing to test beyond the SDK.
type objAPI interface {
	// head returns object user metadata and size; exists=false when missing.
	head(ctx context.Context, key string) (meta map[string]string, size int64, exists bool, err error)
	get(ctx context.Context, key string) (io.ReadCloser, error)
	// PutObject may rewind the body for signing, checksums, and retries. Hostel's
	// chunks and indexes are already bounded in-memory objects, so require that
	// invariant here instead of weakening S3 integrity for non-seekable streams.
	put(ctx context.Context, key string, r io.ReadSeeker, size int64, meta map[string]string) error
	// del removes keys; missing keys are not an error.
	del(ctx context.Context, keys []string) error
	// list returns all keys under prefix.
	list(ctx context.Context, prefix string) ([]string, error)
}

type s3obj struct {
	client *s3.Client
	bucket string
}

func (o *s3obj) head(ctx context.Context, key string) (map[string]string, int64, bool, error) {
	out, err := o.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &o.bucket, Key: &key})
	if err != nil {
		var nf *s3types.NotFound
		if errors.As(err, &nf) {
			return nil, 0, false, nil
		}
		return nil, 0, false, fmt.Errorf("store: head %s: %w", key, err)
	}
	var size int64
	if out.ContentLength != nil {
		size = *out.ContentLength
	}
	return out.Metadata, size, true, nil
}

func (o *s3obj) get(ctx context.Context, key string) (io.ReadCloser, error) {
	out, err := o.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &o.bucket, Key: &key})
	if err != nil {
		return nil, fmt.Errorf("store: get %s: %w", key, err)
	}
	return out.Body, nil
}

func (o *s3obj) put(ctx context.Context, key string, r io.ReadSeeker, size int64, meta map[string]string) error {
	_, err := o.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: &o.bucket, Key: &key, Body: r, ContentLength: &size, Metadata: meta,
	})
	if err != nil {
		return fmt.Errorf("store: put %s: %w", key, err)
	}
	return nil
}

func (o *s3obj) del(ctx context.Context, keys []string) error {
	// DeleteObjects caps at 1000 keys per call.
	for len(keys) > 0 {
		n := min(len(keys), 1000)
		batch := make([]s3types.ObjectIdentifier, n)
		for i := range n {
			batch[i] = s3types.ObjectIdentifier{Key: &keys[i]}
		}
		if _, err := o.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: &o.bucket, Delete: &s3types.Delete{Objects: batch},
		}); err != nil {
			return fmt.Errorf("store: delete batch: %w", err)
		}
		keys = keys[n:]
	}
	return nil
}

func (o *s3obj) list(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	p := s3.NewListObjectsV2Paginator(o.client, &s3.ListObjectsV2Input{Bucket: &o.bucket, Prefix: &prefix})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("store: list %s: %w", prefix, err)
		}
		for _, obj := range page.Contents {
			keys = append(keys, *obj.Key)
		}
	}
	return keys, nil
}
