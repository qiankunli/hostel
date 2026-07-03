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

//go:build !linux

package isolation

// newBwrap on non-Linux falls back to direct: bubblewrap is Linux-only, and
// hostel's dev target (macOS) has no namespaces. Keeps `--isolation suite`
// from failing to boot during local development.
func newBwrap(HostFacts, string) Isolator { return direct{} }
