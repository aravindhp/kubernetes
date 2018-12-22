//go:build linux

/*
Copyright 2022 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package kubelet

import (
	"fmt"
	"strings"
)

// getLoggingCmd returns the journalctl cmd and arguments for the given journalArgs and boot
func getLoggingCmd(a *journalArgs, services []string, boot int64) (string, []string) {
	args := []string{
		"--utc",
		"--no-pager",
		"--output=short-precise",
	}
	if len(a.Since) > 0 {
		args = append(args, fmt.Sprintf("--since=%s", a.Since))
	}
	if len(a.Until) > 0 {
		args = append(args, fmt.Sprintf("--until=%s", a.Until))
	}
	if a.Tail > 0 {
		args = append(args, "--pager-end", fmt.Sprintf("--lines=%d", a.Tail))
	}
	for _, service := range services {
		if len(service) > 0 {
			args = append(args, "--unit="+service)
		}
	}
	if len(a.Pattern) > 0 {
		args = append(args, "--grep="+a.Pattern)
	}

	args = append(args, "--boot", fmt.Sprintf("%d", boot))

	return "journalctl", args
}

// getServiceLogDestinationCmd returns the jorunalctl command that checks if a
// service is using journald
func getServiceLogDestinationCmd(service string) (string, []string) {
	// There is no way to search for a particular service with journalctl
	return "journalctl", []string{"--field", "_SYSTEMD_UNIT"}
}

// checkForNativeLogger checks journalctl output for a service
func checkForNativeLogger(output []byte, service string) bool {
	return strings.Contains(string(output), service+".service")
}

// getCatCmd returns the cat command to output a file
func getCatCmd(logFile string) (string, []string) {
	return "cat", []string{logFile}
}
