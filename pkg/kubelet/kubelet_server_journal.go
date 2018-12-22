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
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"path"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/kubernetes/pkg/api/legacyscheme"
)

var journal = journalServer{}

// journalServer returns text output from the OS specific service logger to view
// from the client. It runs with the privileges of the calling  process
// (the kubelet) and should only be allowed to be invoked by a root user.
type journalServer struct{}

// ServeHTTP translates HTTP query parameters into arguments to be passed
// to journalctl on the current system. It supports content-encoding of
// gzip to reduce total content size.
func (journalServer) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	var out io.Writer = w

	nodeLogQueryOptions := v1.NodeLogQueryOptions{}
	if err := legacyscheme.ParameterCodec.DecodeParameters(
		req.URL.Query(), v1.SchemeGroupVersion, &nodeLogQueryOptions); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	args, err := newJournalArgsFromNodeLogQueryOptions(nodeLogQueryOptions)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// TODO: Also set a response header that indicates how the request's query was resolved,
	// e.g. "kube-log-source: journal://foobar?arg1=value" or "kube-log-source: file:///var/log/foobar.log"
	w.Header().Set("Content-Type", "text/plain;charset=UTF-8")
	if req.Header.Get("Accept-Encoding") == "gzip" {
		w.Header().Set("Content-Encoding", "gzip")

		gz := gzip.NewWriter(out)
		defer gz.Close()
		out = gz
	}
	args.Copy(out)
}

// journalArgs assists in invoking the OS specific logging command.
type journalArgs struct {
	Since    string
	Until    string
	Tail     int64
	Timeout  int
	Boot     *int64
	Services []string
	Pattern  string
}

func newJournalArgsFromNodeLogQueryOptions(nodeLogQueryOptions v1.NodeLogQueryOptions) (*journalArgs, error) {
	services := parseQueryForServices(nodeLogQueryOptions.Query)

	var tail int64
	if nodeLogQueryOptions.TailLines != nil {
		tail = *nodeLogQueryOptions.TailLines
	}

	var since string
	if nodeLogQueryOptions.SinceTime != nil {
		since = strings.TrimSuffix(nodeLogQueryOptions.SinceTime.String(), " +0000 UTC")
	}

	var until string
	if nodeLogQueryOptions.UntilTime != nil {
		until = strings.TrimSuffix(nodeLogQueryOptions.UntilTime.String(), " +0000 UTC")
	}

	// All parameters loaded from the query must be thoroughly sanitized - do
	// not pass query parameters directly to the OS specific command without
	// limiting them as demonstrated above.
	return &journalArgs{
		Services: services,
		Since:    since,
		Until:    until,
		Tail:     tail,
		Boot:     nodeLogQueryOptions.Boot,
		Timeout:  30,
		Pattern:  nodeLogQueryOptions.Pattern,
	}, nil
}

// Copy streams the contents of the OS specific logging command executed
// with the current args to the provided writer, timing out at a.Timeout. If an
// error occurs a line is written to the output.
func (a *journalArgs) Copy(w io.Writer) {
	// set the deadline to the maximum across both runs
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(time.Duration(a.Timeout)*time.Second))
	defer cancel()
	boot := int64(0)
	if a.Boot != nil {
		boot = *a.Boot
	}
	a.copyForBoot(ctx, w, boot)
}

// copyForBoot invokes the OS specific logging command with the  provided args
func (a *journalArgs) copyForBoot(ctx context.Context, w io.Writer, previousBoot int64) {
	if ctx.Err() != nil {
		return
	}
	nativeLoggers, fileLoggers := a.splitNativeVsFileLoggers(ctx)
	if len(nativeLoggers) > 0 {
		a.copyServiceLogs(ctx, w, nativeLoggers, previousBoot)
	}

	// No previous boot logs exist for services that log to a file
	if len(fileLoggers) > 0 && previousBoot == 0 {
		copyFileLogs(ctx, w, fileLoggers)
	}
}

// splitNativeVsFileLoggers checks if each service logs to native OS logs or to a file and returns a list of services
// that log natively vs maybe to a file
func (a *journalArgs) splitNativeVsFileLoggers(ctx context.Context) ([]string, []string) {
	var nativeLoggers []string
	var fileLoggers []string

	for _, service := range a.Services {
		cmdStr, args := getServiceLogDestinationCmd(service)
		cmd := exec.CommandContext(ctx, cmdStr, args...)

		output, err := cmd.CombinedOutput()
		// journalctl will not return an error even if the service is not using journald where as Get-WinEvent will.
		if err != nil {
			if _, ok := err.(*exec.ExitError); ok {
				fileLoggers = append(fileLoggers, service)
				continue
			}
		}

		// Check the journalctl output to figure if the service is using journald or not. This is not needed in the
		// Get-WinEvent case as the command returns an error if a service is not logging to the Application provider.
		if checkForNativeLogger(output, service) {
			nativeLoggers = append(nativeLoggers, service)
		} else {
			fileLoggers = append(fileLoggers, service)
		}
	}
	return nativeLoggers, fileLoggers
}

// copyServiceLogs invokes journalctl or Get-WinEvent with the provided args
func (a *journalArgs) copyServiceLogs(ctx context.Context, w io.Writer, services []string, previousBoot int64) {
	cmdStr, args := getLoggingCmd(a, services, previousBoot)
	cmd := exec.CommandContext(ctx, cmdStr, args...)
	cmd.Stdout = w
	cmd.Stderr = w

	if err := cmd.Run(); err != nil {
		if _, ok := err.(*exec.ExitError); ok {
			return
		}
		if previousBoot == 0 {
			fmt.Fprintf(w, "error: journal output not available\n")
		}
	}
}

// copyFileLogs loops over all the services and attempts to collect the file logs of each service
func copyFileLogs(ctx context.Context, w io.Writer, services []string) {
	if ctx.Err() != nil {
		return
	}

	for _, service := range services {
		if len(service) > 0 {
			heuristicsCat(ctx, w, service)
		}
	}
}

// heuristicsCat attempts to collect logs from either
// /var/log/service.log or
// /var/log/service/service.log or
// /var/log/service*INFO.* or
// /var/log/service*INFO or
// /var/log/service/service*INFO.* or
// /var/log/service/service*INFO
// in that order stopping on first success.
func heuristicsCat(ctx context.Context, w io.Writer, service string) {
	logDir := "/var/log/"
	logFiles := [6]string{
		path.Join(logDir, fmt.Sprintf("%s.log", service)),
		path.Join(logDir, service, fmt.Sprintf("%s.log", service)),
		path.Join(logDir, fmt.Sprintf("%s*INFO.*", service)),
		path.Join(logDir, fmt.Sprintf("%s*INFO", service)),
		path.Join(logDir, service, fmt.Sprintf("%s*INFO.*", service)),
		path.Join(logDir, service, fmt.Sprintf("%s*INFO", service)),
	}

	for _, logFile := range logFiles {
		if err := cat(ctx, w, logFile); err == nil {
			break
		}
	}
}

// cat returns the contents of the given logFile
func cat(ctx context.Context, w io.Writer, logFile string) error {
	cmdStr, args := getCatCmd(logFile)
	cmd := exec.CommandContext(ctx, cmdStr, args...)
	cmd.Stdout = w
	cmd.Stderr = w

	return cmd.Run()
}

// parseQueryForServices traverses the query slice and returns the list of services
func parseQueryForServices(query []string) (services []string) {
	for _, q := range query {
		if strings.ContainsAny(q, "/\\") {
			continue
		} else {
			services = append(services, q)
		}
	}
	return
}
