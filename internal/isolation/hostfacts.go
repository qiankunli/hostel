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

package isolation

import "os/exec"

// HostFacts is the boot-time snapshot of what THIS host offers the isolation
// resolver: the probed, read-only truths behind the ceiling. Collected once in
// New and handed to every mechanism, so they share one /proc read instead of
// each re-probing; also surfaced in /healthz so an operator can see why a host
// tops out where it does without shelling into it.
//
// A fact is a fast-fail PRE-CHECK, never the verdict: it can rule a mechanism
// OUT (no Landlock ABI → skip) but only the mechanism's own smoke can rule it
// IN, because declared capability ≠ real enforcement (a Landlock ABI can
// BestEffort no-op; caps present can still fail setuid under seccomp). So a
// mechanism reads facts for the cheap half of its probe and keeps its smoke for
// the authoritative half.
type HostFacts struct {
	KernelRelease      string `json:"kernel_release"`      // uname release ("" off Linux)
	EffectiveCaps      uint64 `json:"effective_caps"`      // CapEff bitmask from /proc/self/status
	LandlockABI        int    `json:"landlock_abi"`        // Landlock ABI version; 0 = absent/unsupported
	BwrapPath          string `json:"bwrap_path"`          // resolved bubblewrap binary ("" = not found)
	UnprivilegedUserns bool   `json:"unprivileged_userns"` // kernel hint: unprivileged user namespaces allowed
	CgroupV2           bool   `json:"cgroup_v2"`           // unified cgroup v2 hierarchy present
	// AppArmorProfile is this process's AppArmor confinement label ("" = none /
	// unconfined / AppArmor absent; e.g. "cri-containerd.apparmor.d (enforce)").
	// The fact behind "userns is allowed yet bwrap still dies at mount": in
	// k8s, containerd's default profile denies mount(2), vetoing suite even
	// though the kernel permits unprivileged userns. Deliberately a DISCLOSED
	// fact rather than a deployment requirement — customer clusters can't be
	// assumed to accept an AppArmor-exemption annotation (non-AppArmor distro
	// nodes may even reject annotated pods), so hostel degrades honestly and
	// this field tells the operator why.
	AppArmorProfile string `json:"apparmor_profile"`
}

// HasCap reports whether capability bit (e.g. capSETUID) is in the effective
// set. bit must be < 64; a larger value shifts out to a well-defined 0 (Go
// shift semantics), so HasCap returns false rather than crashing — but that's a
// misuse, not a real "cap absent". Always false off Linux, where EffectiveCaps
// is 0.
func (f HostFacts) HasCap(bit uint) bool { return f.EffectiveCaps&(1<<bit) != 0 }

// collectHostFacts probes the host once at boot. The bubblewrap lookup is
// cross-platform; the kernel/caps/Landlock/userns/cgroup facts are filled
// per-OS by osFacts (all zero off Linux).
func collectHostFacts() HostFacts {
	f := osFacts()
	if p, err := exec.LookPath("bwrap"); err == nil {
		f.BwrapPath = p
	}
	return f
}
