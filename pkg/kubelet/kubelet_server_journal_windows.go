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

// getLoggingCmd returns the powershell cmd and arguments for the given journalArgs and boot
func getLoggingCmd(a *journalArgs, services []string, boot int64) (string, []string) {
	// The WinEvent log does not support querying by boot
	// Set the cmd to return true on windows in case boot is not 0
	if boot != 0 {
		return "PowerShell.exe", []string{"cd"}
	}

	args := []string{
		"-NonInteractive",
		"-ExecutionPolicy", "Bypass",
		"-Command",
	}

	psCmd := "Get-WinEvent -FilterHashtable @{LogName='Application'"
	if len(a.Since) > 0 {
		psCmd += fmt.Sprintf("; StartTime='%s'", a.Since)
	}
	if len(a.Until) > 0 {
		psCmd += fmt.Sprintf("; EndTime='%s'", a.Until)
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
	if a.Tail > 0 {
		psCmd += fmt.Sprintf(" -MaxEvents %d", a.Tail)
	}
	psCmd += " | Sort-Object TimeCreated"
	if len(a.Pattern) > 0 {
		psCmd += fmt.Sprintf(" | Where-Object -Property Message -Match %s", a.Pattern)
	}
	psCmd += " | Format-Table -AutoSize -Wrap"

	args = append(args, psCmd)

	return powershellExe, args
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
func checkForNativeLogger(output []byte, service string) bool {
	// Get-WinEvent returns an error if a service is not present and is a better indicator rather than parsing the
	// output which tends to be variable across installations
	return true
}

// getCatCmd returns the cat command to output a file
func getCatCmd(logFile string) (string, []string) {
	args := []string{
		"-NonInteractive",
		"-ExecutionPolicy", "Bypass",
		"-Command",
		"Get-Content " + logFile,
	}
	return powershellExe, args
}
