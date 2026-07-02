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
	"os"
	"path"
	"strings"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// s3Store keeps one gzipped tarball per bed: <prefix>/<bedID>.tar.gz.
// One object per bed makes Persist atomic (S3 PUT is all-or-nothing) and
// versionable; incremental sync is a later optimization (docs/persistence.md §3).
type s3Store struct {
	client *s3.Client
	bucket string
	prefix string
}

// s3OpTimeout bounds a single snapshot transfer. Generous: snapshots can be
// hundreds of MB on slow links; lifecycle callers pass Background contexts.
const s3OpTimeout = 5 * time.Minute

func newS3(ctx context.Context, cfg Config) (Store, error) {
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: aws config: %w", err)
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = &cfg.Endpoint
			// S3-compatible stores (MinIO/TOS/Ceph) generally want
			// path-style addressing rather than virtual-hosted buckets.
			o.UsePathStyle = true
		}
	})
	return &s3Store{client: client, bucket: cfg.Bucket, prefix: cfg.Prefix}, nil
}

func (s *s3Store) Name() string { return "s3" }

func (s *s3Store) key(bedID string) string {
	return path.Join(s.prefix, bedID+".tar.gz")
}

func (s *s3Store) Exists(ctx context.Context, bedID string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, s3OpTimeout)
	defer cancel()
	_, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: &s.bucket, Key: strPtr(s.key(bedID)),
	})
	if err != nil {
		var nf *s3types.NotFound
		if errors.As(err, &nf) {
			return false, nil
		}
		return false, fmt.Errorf("store: head %s: %w", s.key(bedID), err)
	}
	return true, nil
}

func (s *s3Store) Persist(ctx context.Context, bedID, dir string) error {
	ctx, cancel := context.WithTimeout(ctx, s3OpTimeout)
	defer cancel()

	// Stage the tarball in a temp file first: S3 PUT wants a known length,
	// and it keeps the pack step's disk/CPU cost off the network timeout.
	tmp, err := os.CreateTemp("", "hostel-snap-*.tar.gz")
	if err != nil {
		return fmt.Errorf("store: temp snapshot: %w", err)
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	// Top-level *.local files are host-private by convention and stay out of
	// the portable snapshot (docs/persistence.md §4).
	skipLocal := func(rel string) bool {
		return !strings.Contains(rel, "/") && strings.HasSuffix(rel, ".local")
	}
	if err := packDir(dir, tmp, skipLocal); err != nil {
		return fmt.Errorf("store: pack %s: %w", bedID, err)
	}
	if _, err := tmp.Seek(0, 0); err != nil {
		return err
	}
	info, err := tmp.Stat()
	if err != nil {
		return err
	}
	size := info.Size()
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        &s.bucket,
		Key:           strPtr(s.key(bedID)),
		Body:          tmp,
		ContentLength: &size,
	})
	if err != nil {
		return fmt.Errorf("store: put %s: %w", s.key(bedID), err)
	}
	return nil
}

func (s *s3Store) Restore(ctx context.Context, bedID, dir string) error {
	ctx, cancel := context.WithTimeout(ctx, s3OpTimeout)
	defer cancel()
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: &s.bucket, Key: strPtr(s.key(bedID)),
	})
	if err != nil {
		return fmt.Errorf("store: get %s: %w", s.key(bedID), err)
	}
	defer out.Body.Close()
	if err := unpackDir(out.Body, dir); err != nil {
		return fmt.Errorf("store: unpack %s: %w", bedID, err)
	}
	return nil
}

func (s *s3Store) Delete(ctx context.Context, bedID string) error {
	ctx, cancel := context.WithTimeout(ctx, s3OpTimeout)
	defer cancel()
	// S3 DeleteObject on a missing key succeeds — matching the interface.
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: &s.bucket, Key: strPtr(s.key(bedID)),
	})
	if err != nil {
		return fmt.Errorf("store: delete %s: %w", s.key(bedID), err)
	}
	return nil
}

func strPtr(s string) *string { return &s }
