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

// Package extensions hosts in-bed CLI tools compiled into the hostel binary,
// dispatched multi-call style (busybox pattern): the image symlinks each tool
// name to the hostel executable and main() routes on argv[0]. One binary keeps
// the single-binary deployment promise — adding a tool costs a package here,
// one registry line, and a symlink in the image; never another COPY.
package extensions

import "github.com/qiankunli/hostel/extensions/playwright"

// registry maps the argv[0] basename to the tool entrypoint. The returned int
// is the process exit code (an entrypoint may also exec-replace the process
// and never return).
var registry = map[string]func(args []string) int{
	"playwright":     playwright.Run,
	"playwright-cli": playwright.Run,
}

// Lookup resolves an argv[0] basename to its tool entrypoint.
func Lookup(name string) (func(args []string) int, bool) {
	run, ok := registry[name]
	return run, ok
}
