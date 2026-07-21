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
	"testing"
)

func TestNewS3ClientAddressingStyle(t *testing.T) {
	t.Setenv("AWS_REGION", "us-east-1")
	const endpoint = "https://tos-s3-cn-beijing.volces.com"

	for _, pathStyle := range []bool{false, true} {
		client, err := newS3Client(context.Background(), Config{
			Endpoint:  endpoint,
			PathStyle: pathStyle,
		})
		if err != nil {
			t.Fatalf("newS3Client(pathStyle=%v): %v", pathStyle, err)
		}
		opts := client.Options()
		if opts.BaseEndpoint == nil || *opts.BaseEndpoint != endpoint {
			t.Fatalf("BaseEndpoint = %v, want %q", opts.BaseEndpoint, endpoint)
		}
		if opts.UsePathStyle != pathStyle {
			t.Fatalf("UsePathStyle = %v, want %v", opts.UsePathStyle, pathStyle)
		}
	}
}
