//go:build windows

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

const powershellExe = "PowerShell.exe"

// getLoggingCmd returns the powershell cmd and arguments for the given nodeLogQuery and boot
func getLoggingCmd(n *nodeLogQuery, services []string) (string, []string, error) {
	args := []string{
		"-NonInteractive",
		"-ExecutionPolicy", "Bypass",
		"-Command",
	}

	psCmd := "Get-WinEvent -FilterHashtable @{LogName='Application'"
	if n.SinceTime != nil {
		psCmd += fmt.Sprintf("; StartTime='%s'", strings.TrimSuffix(n.SinceTime.UTC().String(), " +0000 UTC"))
	}
	if n.UntilTime != nil {
		psCmd += fmt.Sprintf("; EndTime='%s'", strings.TrimSuffix(n.UntilTime.UTC().String(), " +0000 UTC"))
	}
	var providers []string
	for _, service := range services {
		if len(service) > 0 {
			providers = append(providers, "'"+service+"'")
		}
	}
	if len(providers) > 0 {
		psCmd += fmt.Sprintf("; ProviderName=%s", strings.Join(providers, ","))
	}
	psCmd += "}"
	if n.TailLines != nil {
		psCmd += fmt.Sprintf(" -MaxEvents %d", *n.TailLines)
	}
	psCmd += " | Sort-Object TimeCreated"
	if len(n.Pattern) > 0 {
		psCmd += fmt.Sprintf(" | Where-Object -Property Message -Match %s", n.Pattern)
	}
	psCmd += " | Format-Table -AutoSize -Wrap"

	args = append(args, psCmd)

	return powershellExe, args, nil
}

// getServiceLogDestinationCmd returns the PowerShell Get-WinEvent cmdlet that checks if a service is a registered
// provider
func getServiceLogDestinationCmd(service string) (string, []string) {
	getWinEventCmd := fmt.Sprintf("Get-WinEvent -ListProvider %s | Format-Table -AutoSize", service)
	args := []string{
		"-NonInteractive",
		"-ExecutionPolicy", "Bypass",
		"-Command",
		getWinEventCmd,
	}
	return powershellExe, args
}

// checkForNativeLogger always returns true for Windows
func checkForNativeLogger(ctx context.Context, service string) bool {
	cmdStr, args := getServiceLogDestinationCmd(service)
	cmd := exec.CommandContext(ctx, cmdStr, args...)

	output, err := cmd.CombinedOutput()
	if err != nil {
		if _, ok := err.(*exec.ExitError); ok {
			return false
		}
	}
	return true
}
