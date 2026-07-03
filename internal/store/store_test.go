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

import "testing"

func TestBackendSelection(t *testing.T) {
	st, err := New(t.Context(), Config{Backend: "noop"})
	if err != nil || st.Name() != "noop" {
		t.Fatalf("New noop: %v %v", st, err)
	}
	info, err := st.Stat(t.Context(), "x")
	if err != nil || info != nil {
		t.Fatalf("noop Stat = %v %v", info, err)
	}
	for _, backend := range []string{"s3", "cas" /* alias */} {
		if _, err := New(t.Context(), Config{Backend: backend}); err == nil {
			t.Fatalf("%s without bucket should fail", backend)
		}
	}
	if _, err := New(t.Context(), Config{Backend: "bogus"}); err == nil {
		t.Fatal("unknown backend should fail")
	}
	// auto resolves by intent: bucket set → s3 (cas layout), no bucket → noop.
	if st, err := New(t.Context(), Config{Backend: "auto"}); err != nil || st.Name() != "noop" {
		t.Fatalf("auto without bucket = %v, %v; want noop", st, err)
	}
	if st, err := New(t.Context(), Config{Backend: "auto", Bucket: "b"}); err != nil || st.Name() != "s3" {
		t.Fatalf("auto with bucket = %v, %v; want s3", st, err)
	}
}
