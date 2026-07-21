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
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
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

func TestS3ObjPutOverHTTP(t *testing.T) {
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_ACCESS_KEY_ID", "test-access-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test-secret-key")

	want := []byte("hostel snapshot index")
	var got []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("method = %s, want PUT", r.Method)
		}
		var err error
		got, err = io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client, err := newS3Client(context.Background(), Config{Endpoint: server.URL, PathStyle: true})
	if err != nil {
		t.Fatalf("newS3Client: %v", err)
	}
	obj := &s3obj{client: client, bucket: "bucket"}
	if err := obj.put(context.Background(), "bed/index.caibx", bytes.NewReader(want), int64(len(want)), nil); err != nil {
		t.Fatalf("put over HTTP: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("body = %q, want %q", got, want)
	}
}
