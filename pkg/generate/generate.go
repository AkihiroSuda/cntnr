// Copyright © 2017 Max Goltzsche
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

package generate

import (
	"sort"
	"strings"
	"time"

	ispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/opencontainers/runc/libcontainer/specconv"
	rspecs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/opencontainers/runtime-tools/generate"
	"github.com/opencontainers/runtime-tools/generate/seccomp"
	"github.com/syndtr/gocapability/capability"
)

type SpecBuilder struct {
	generate.Generator
	entrypoint []string
	cmd        []string
	prootPath  string
}

func NewSpecBuilder() SpecBuilder {
	return SpecBuilder{Generator: generate.New()}
}

func FromSpec(spec *rspecs.Spec) SpecBuilder {
	return SpecBuilder{Generator: generate.NewFromSpec(spec)}
}

func (b *SpecBuilder) ToRootless() {
	specconv.ToRootless(b.Spec())
}

func (b *SpecBuilder) UseHostNetwork() {
	b.RemoveLinuxNamespace(rspecs.NetworkNamespace)
	opts := []string{"bind", "mode=0444", "nosuid", "noexec", "nodev", "ro"}
	b.AddBindMount("/etc/hosts", "/etc/hosts", opts)
	b.AddBindMount("/etc/resolv.conf", "/etc/resolv.conf", opts)
}

func (b *SpecBuilder) AddAllProcessCapabilities() {
	// Add all capabilities
	all := capability.List()
	caps := make([]string, len(all))
	for i, c := range all {
		caps[i] = "CAP_" + strings.ToUpper(c.String())
	}
	c := b.Spec().Process.Capabilities
	c.Effective = caps
	c.Permitted = caps
	c.Bounding = caps
	c.Ambient = caps
	c.Inheritable = caps
}

func (b *SpecBuilder) DropAllProcessCapabilities() {
	caps := []string{}
	c := b.Spec().Process.Capabilities
	c.Effective = caps
	c.Permitted = caps
	c.Bounding = caps
	c.Ambient = caps
	c.Inheritable = caps
}

// Derives a reasonable default seccomp from the current spec
func (b *SpecBuilder) SetLinuxSeccompDefault() {
	spec := b.Spec()
	spec.Linux.Seccomp = seccomp.DefaultProfile(spec)
}

func (b *SpecBuilder) SetLinuxSeccompUnconfined() {
	spec := b.Spec()
	profile := seccomp.DefaultProfile(spec)
	profile.DefaultAction = rspecs.ActAllow
	profile.Syscalls = nil
	spec.Linux.Seccomp = profile
}

func (b *SpecBuilder) SetLinuxSeccomp(profile *rspecs.LinuxSeccomp) {
	spec := b.Spec()
	if spec.Linux == nil {
		spec.Linux = &rspecs.Linux{}
	}
	spec.Linux.Seccomp = profile
}

func (b *SpecBuilder) AddExposedPorts(ports []string) {
	// Merge exposedPorts annotation
	exposedPortsAnn := ""
	if b.Spec().Annotations != nil {
		exposedPortsAnn = b.Spec().Annotations["org.opencontainers.image.exposedPorts"]
	}
	exposed := map[string]bool{}
	if exposedPortsAnn != "" {
		for _, exposePortStr := range strings.Split(exposedPortsAnn, ",") {
			exposed[strings.Trim(exposePortStr, " ")] = true
		}
	}
	for _, e := range ports {
		exposed[strings.Trim(e, " ")] = true
	}
	if len(exposed) > 0 {
		exposecsv := make([]string, len(exposed))
		i := 0
		for k := range exposed {
			exposecsv[i] = k
			i++
		}
		sort.Strings(exposecsv)
		b.AddAnnotation("org.opencontainers.image.exposedPorts", strings.Join(exposecsv, ","))
	}
}

func (b *SpecBuilder) SetPRootPath(prootPath string) {
	b.prootPath = prootPath
	spec := b.Spec()
	// This has been derived from https://github.com/AkihiroSuda/runrootless/blob/b9a7df0120a7fee15c0223fd0fbc8c3885edd9b3/bundle/spec.go
	spec.Mounts = append(spec.Mounts,
		rspecs.Mount{
			Destination: "/dev/proot",
			Type:        "tmpfs",
			Source:      "tmpfs",
			Options:     []string{"exec", "mode=755", "size=32256k"},
		},
		rspecs.Mount{
			Destination: "/dev/proot/proot",
			Type:        "bind",
			Source:      prootPath,
			Options:     []string{"bind", "ro"},
		},
	)
	spec.Process.Env = append(spec.Process.Env, "PROOT_TMP_DIR=/dev/proot", "PROOT_NO_SECCOMP=1")
	b.AddProcessCapability("CAP_" + capability.CAP_SYS_PTRACE.String())
	b.applyEntrypoint()
	b.SetLinuxSeccompDefault()
}

func (b *SpecBuilder) SetProcessEntrypoint(v []string) {
	b.entrypoint = v
	b.cmd = nil
	b.applyEntrypoint()
}

func (b *SpecBuilder) SetProcessCmd(v []string) {
	b.cmd = v
	b.applyEntrypoint()
}

func (b *SpecBuilder) applyEntrypoint() {
	var args []string
	if b.entrypoint != nil || b.cmd != nil {
		if b.entrypoint != nil && b.cmd != nil {
			args = append(b.entrypoint, b.cmd...)
		} else if b.entrypoint != nil {
			args = b.entrypoint
		} else {
			args = b.cmd
		}
	} else {
		args = []string{}
	}
	if b.prootPath != "" {
		args = append([]string{"/dev/proot/proot"}, args...)
	}
	b.SetProcessArgs(args)
}

// See image to runtime spec conversion rules: https://github.com/opencontainers/image-spec/blob/master/conversion.md
func (b *SpecBuilder) ApplyImage(img ispecs.Image) {
	cfg := &img.Config

	// Entrypoint
	b.SetProcessEntrypoint(cfg.Entrypoint)
	b.SetProcessCmd(cfg.Cmd)

	// Env
	if len(cfg.Env) > 0 {
		for _, e := range cfg.Env {
			kv := strings.SplitN(e, "=", 2)
			k := kv[0]
			v := ""
			if len(kv) == 2 {
				v = kv[1]
			}
			b.AddProcessEnv(k, v)
		}
	}

	// Working dir
	if cfg.WorkingDir != "" {
		b.SetProcessCwd(cfg.WorkingDir)
	}

	// Annotations
	if cfg.Labels != nil {
		for k, v := range cfg.Labels {
			b.AddAnnotation(k, v)
		}
	}
	// TODO: extract annotations also from image index and manifest
	if img.Author != "" {
		b.AddAnnotation("org.opencontainers.image.author", img.Author)
	}
	if img.Created != nil && !time.Unix(0, 0).Equal(*img.Created) {
		b.AddAnnotation("org.opencontainers.image.created", (*img.Created).String())
	}
	if img.Config.StopSignal != "" {
		b.AddAnnotation("org.opencontainers.image.stopSignal", img.Config.StopSignal)
	}
	if cfg.ExposedPorts != nil {
		ports := make([]string, len(cfg.ExposedPorts))
		i := 0
		for k := range cfg.ExposedPorts {
			ports[i] = k
			i++
		}
		b.AddAnnotation("org.opencontainers.image.exposedPorts", strings.Join(ports, ","))
	}
}
