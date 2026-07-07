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

//go:build linux

package isolation

import (
	"os"
	"strconv"
	"strings"

	lls "github.com/landlock-lsm/go-landlock/landlock/syscall"
	"golang.org/x/sys/unix"
)

// Linux capability bit numbers the isolation mechanisms gate on.
const (
	capCHOWN  uint = 0
	capSETGID uint = 6
	capSETUID uint = 7
)

// osFacts fills the Linux-only host facts. Each probe degrades to a zero value
// on error, which simply makes the dependent mechanism report unavailable — the
// resolver then floors honestly.
func osFacts() HostFacts {
	var f HostFacts
	if v, err := lls.LandlockGetABIVersion(); err == nil {
		f.LandlockABI = int(v)
	}
	f.EffectiveCaps = readEffectiveCaps()
	f.KernelRelease = kernelRelease()
	f.UnprivilegedUserns = unprivilegedUserns()
	f.CgroupV2 = cgroupV2()
	f.AppArmorProfile = apparmorProfile()
	return f
}

// apparmorProfile reads this process's AppArmor confinement label. "unconfined"
// and absence both normalize to "" — only an actual confining profile is a fact
// worth surfacing.
//
// The AppArmor-specific attr node (Linux 4.17+) is authoritative: if it exists,
// AppArmor is the active LSM and its value is the answer. The legacy shared node
// (/proc/self/attr/current) is LSM-ambiguous — SELinux writes its own label
// there too — so we read it only when AppArmor is actually present, else an
// SELinux-only host would mislabel a SELinux context as an AppArmor profile.
func apparmorProfile() string {
	if data, err := os.ReadFile("/proc/self/attr/apparmor/current"); err == nil {
		return normalizeAppArmorLabel(string(data))
	}
	if _, err := os.Stat("/sys/kernel/security/apparmor"); err == nil {
		if data, err := os.ReadFile("/proc/self/attr/current"); err == nil {
			return normalizeAppArmorLabel(string(data))
		}
	}
	return ""
}

func normalizeAppArmorLabel(raw string) string {
	label := strings.TrimSpace(raw)
	if label == "" || label == "unconfined" {
		return ""
	}
	return label
}

// readEffectiveCaps parses CapEff from /proc/self/status (0 on any error).
func readEffectiveCaps() uint64 {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(line, "CapEff:"); ok {
			caps, _ := strconv.ParseUint(strings.TrimSpace(v), 16, 64)
			return caps
		}
	}
	return 0
}

func kernelRelease() string {
	var u unix.Utsname
	if err := unix.Uname(&u); err != nil {
		return ""
	}
	return unix.ByteSliceToString(u.Release[:])
}

// unprivilegedUserns reads the Debian/Ubuntu knob. It's only a /healthz hint:
// kernels without the knob still allow user namespaces, so absence reads as
// false here while bwrap's boot smoke remains the authoritative suite check.
func unprivilegedUserns() bool {
	data, err := os.ReadFile("/proc/sys/kernel/unprivileged_userns_clone")
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(data)) == "1"
}

func cgroupV2() bool {
	_, err := os.Stat("/sys/fs/cgroup/cgroup.controllers")
	return err == nil
}
