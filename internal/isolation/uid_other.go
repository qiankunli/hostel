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

import "fmt"

// AsUserArg exists on all platforms so main can dispatch it uniformly.
const AsUserArg = "__asuser"

// newUID: uid isolation relies on Linux setuid/setgid + /proc caps. Report room
// as unavailable elsewhere.
func newUID(string) Isolator { return unavailable{name: "uid", lvl: Room} }

// ApplyAsUser should never run off Linux (no uid isolator can be chosen).
func ApplyAsUser(int, string) error { return fmt.Errorf("uid isolation: unsupported on this platform") }
