package main

import "testing"

func TestEnvironmentSeparatesPlatformMatchFromValidatedBackup(t *testing.T) {
	arm := evaluateEnvironment("linux", "arm64", "5.10.198", "ID=ubuntu\nVERSION_ID=\"20.04\"\n")
	if arm.PlatformMatch || arm.BackupValidated || len(arm.Issues) != 3 || arm.DistributionVersion != "20.04" {
		t.Fatal("ARM/Focal custom kernel must be explained as unsupported", arm)
	}
	x86 := evaluateEnvironment("linux", "amd64", "6.8.0-147-generic", "ID=ubuntu\nVERSION_ID=\"24.04\"\n")
	if !x86.PlatformMatch || x86.BackupValidated || len(x86.Issues) != 0 {
		t.Fatal("matching x86 platform must not claim validated backup", x86)
	}
	unknown := evaluateEnvironment("windows", "amd64", "", "")
	if unknown.PlatformMatch || unknown.BackupValidated || unknown.Distribution != "" {
		t.Fatal("missing host information must not imply support", unknown)
	}
}
