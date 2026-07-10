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

package playwright

import (
	"reflect"
	"testing"
)

func TestRoute(t *testing.T) {
	const ep = "ws://127.0.0.1:8872/v1/cdp?bed=b1&t=s"
	cases := []struct {
		name string
		args []string
		cdp  string
		want action
	}{
		{"install is a noop", []string{"install", "--with-deps", "chromium"}, ep,
			action{kind: kindNoop, argv: []string{"install", "--with-deps", "chromium"}}},
		{"install-deps is a noop", []string{"install-deps"}, ep,
			action{kind: kindNoop, argv: []string{"install-deps"}}},
		{"open with url becomes goto", []string{"open", "https://example.com"}, ep,
			action{kind: kindCLI, argv: []string{"goto", "https://example.com"}}},
		{"navigate with url becomes goto", []string{"navigate", "https://example.com"}, ep,
			action{kind: kindCLI, argv: []string{"goto", "https://example.com"}}},
		{"bare open passes through", []string{"open"}, ep,
			action{kind: kindCLIDirect, argv: []string{"open"}}},
		{"open with non-url passes through", []string{"open", "--headed"}, ep,
			action{kind: kindCLIDirect, argv: []string{"open", "--headed"}}},
		{"page verb routes to cli", []string{"screenshot", "--filename=x.png"}, ep,
			action{kind: kindCLI, argv: []string{"screenshot", "--filename=x.png"}}},
		{"bare attach gets the bed endpoint", []string{"attach"}, ep,
			action{kind: kindCLIDirect, argv: []string{"attach", "--cdp=" + ep}}},
		{"attach with explicit cdp untouched", []string{"attach", "--cdp=ws://other"}, ep,
			action{kind: kindCLIDirect, argv: []string{"attach", "--cdp=ws://other"}}},
		{"bare attach outside a bed untouched", []string{"attach"}, "",
			action{kind: kindCLIDirect, argv: []string{"attach"}}},
		{"unknown verb falls to classic cli", []string{"codegen", "https://example.com"}, ep,
			action{kind: kindPW, argv: []string{"codegen", "https://example.com"}}},
		{"no args shows cli help", nil, ep,
			action{kind: kindCLIDirect, argv: []string{"--help"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := route(tc.args, tc.cdp)
			if got.kind != tc.want.kind || !reflect.DeepEqual(got.argv, tc.want.argv) {
				t.Fatalf("route(%v) = %+v, want %+v", tc.args, got, tc.want)
			}
		})
	}
}

// Every page verb must route to the session path — a verb quietly dropping to
// the classic CLI would launch a stray browser instead of the bed slice.
func TestRouteAllPageVerbs(t *testing.T) {
	for verb := range pageVerbs {
		if got := route([]string{verb}, "ws://x"); got.kind != kindCLI {
			t.Fatalf("verb %q routed to kind %d, want kindCLI", verb, got.kind)
		}
	}
}
