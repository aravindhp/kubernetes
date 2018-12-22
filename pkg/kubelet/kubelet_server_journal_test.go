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
	"reflect"
	"runtime"
	"testing"
)

func Test_journalArgs_Args(t *testing.T) {
	tests := []struct {
		name        string
		args        journalArgs
		wantLinux   []string
		wantWindows []string
		wantOtherOS []string
	}{
		{
			args:        journalArgs{},
			wantLinux:   []string{"--utc", "--no-pager", "--output=short-precise", "--boot", "0"},
			wantWindows: []string{"-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", "Get-WinEvent -FilterHashtable @{LogName='Application'} | Sort-Object TimeCreated | Format-Table -AutoSize -Wrap"},
			wantOtherOS: []string{"Operating System Not Supported"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, got := getLoggingCmd(&tt.args, []string{}, 0)
			switch os := runtime.GOOS; os {
			case "linux":
				if !reflect.DeepEqual(got, tt.wantLinux) {
					t.Errorf("getLoggingCmd(journalArgs{}, 0) = %v, want %v", got, tt.wantLinux)
				}
			case "windows":
				if !reflect.DeepEqual(got, tt.wantWindows) {
					t.Errorf("getLoggingCmd(journalArgs{}, 0) = %v, want %v", got, tt.wantWindows)
				}
			default:
				if !reflect.DeepEqual(got, tt.wantOtherOS) {
					t.Errorf("getLoggingCmd(journalArgs{}, 0) = %v, want %v", got, tt.wantWindows)
				}
			}
		})
	}
}
