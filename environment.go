package main

import (
	"os"
	"regexp"
	"runtime"
	"strings"
)

type Environment struct {
	OS                  string   `json:"os"`
	Architecture        string   `json:"architecture"`
	Distribution        string   `json:"distribution"`
	DistributionVersion string   `json:"distribution_version"`
	Kernel              string   `json:"kernel"`
	PlatformMatch       bool     `json:"platform_match"`
	BackupValidated     bool     `json:"backup_validated"`
	AgentSocketPresent  bool     `json:"agent_socket_present"`
	Issues              []string `json:"issues"`
	Message             string   `json:"message"`
}

// This describes the console host, not a certification of the privileged
// agent's environment. The agent/driver must perform their own validation.
func inspectEnvironment(socket string) Environment {
	kernel, _ := os.ReadFile("/proc/sys/kernel/osrelease")
	release, _ := os.ReadFile("/etc/os-release")
	result := evaluateEnvironment(runtime.GOOS, runtime.GOARCH, strings.TrimSpace(string(kernel)), string(release))
	info, err := os.Stat(socket)
	result.AgentSocketPresent = err == nil && info.Mode()&os.ModeSocket != 0
	return result
}

func evaluateEnvironment(system, arch, kernel, release string) Environment {
	values := map[string]string{}
	for _, line := range strings.Split(release, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			values[key] = strings.Trim(strings.TrimSpace(value), "\"'")
		}
	}
	result := Environment{OS: system, Architecture: arch, Distribution: values["ID"], DistributionVersion: values["VERSION_ID"], Kernel: kernel, Issues: []string{}}
	if system != "linux" {
		result.Issues = append(result.Issues, "当前 CBT 适配目标要求 Linux。")
	}
	if arch != "amd64" {
		result.Issues = append(result.Issues, "当前 CBT 适配目标仅包含 x86_64 / amd64；ARM64 可体验控制台，内核备份未适配。")
	}
	if values["ID"] != "ubuntu" || values["VERSION_ID"] != "24.04" {
		result.Issues = append(result.Issues, "当前 CBT 适配目标要求 Ubuntu 24.04；其他发行版与版本未适配。")
	}
	if !regexp.MustCompile(`^6\.8\.0-[0-9]+-generic$`).MatchString(kernel) {
		result.Issues = append(result.Issues, "当前 CBT 适配目标要求 GA 6.8.0-数字-generic 内核。")
	}
	result.PlatformMatch = len(result.Issues) == 0
	result.Message = "当前设备可体验控制台，但不在 CBT 驱动的适配目标内。"
	if result.PlatformMatch {
		result.Message = "当前设备符合 CBT 适配目标；后台服务尚需单独部署，真实增量恢复验收仍未通过。"
	}
	// Matching architecture, distribution and kernel never implies that the
	// known full/incremental recovery acceptance failure has been resolved.
	result.BackupValidated = false
	return result
}
