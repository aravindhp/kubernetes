/*
Copyright 2021 The Kubernetes Authors.

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

package logs

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/reference"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/polymorphichelpers"
	"k8s.io/kubectl/pkg/scheme"
	"k8s.io/kubectl/pkg/util"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"
)

const (
	logsNodesUsageStr         = "node-logs ([--role ROLE] | [--label LABEL] | [NODE...])"
	logsNodesWriterBufferSize = 16384
	logsNodesReaderBufferSize = 4096
)

var (
	logsNodesLong = templates.LongDesc(i18n.T(`
		Experimental: Display and filter node logs when the kubelet NodeLogs feature
		flag is set.

		This command retrieves logs for the node. The default mode is to query the
		systemd journal or WinEvent entries on supported operating systems,
		which allows searching, time based filtering, and service based filtering.
		You may also query log files available under /var/logs/ and view those contents
		directly.

		Node logs may contain sensitive output and so are limited to privileged node
		administrators. The system:node-admins role grants this permission by default.
	`))

	logsNodesExample = templates.Examples(i18n.T(`
		# Show kubelet and crio journald logs from all masters
		kubectl alpha node-logs --role master --query kubelet --query crio

		# Show kubelet log file (/var/log/kubelet/kubelet.log) from all Windows worker nodes
		kubectl alpha node-logs --label kubernetes.io/os=windows --query kubelet

		# Show docker WinEvent logs from a specific Windows worker node
		kubectl alpha node-logs <node-name> --query docker

		# Show crio journald logs from a specific Linux node
		kubectl alpha node-logs <node-name> --query crio

		# Show content of file foo.log that is present in /var/log
		kubectl alpha node-logs <node-name> --query /foo.log
	`))
)

type NodeLogsCmdFlags struct {
	Factory cmdutil.Factory

	Query     []string
	Pattern   string
	SinceTime string
	UntilTime string
	Boot      int64
	TailLines int64
	Role      string
	Selector  string
	Raw       bool
	Unify     bool

	genericclioptions.IOStreams
}

func NewNodeLogsCmdFlags(factory cmdutil.Factory, streams genericclioptions.IOStreams) *NodeLogsCmdFlags {
	return &NodeLogsCmdFlags{
		Factory:   factory,
		IOStreams: streams,
	}
}

type NodeLogsCmdOptions struct {
	Resources []string
	Selector  string
	Role      string

	Query     []string
	Pattern   string
	Boot      *int64
	SinceTime *metav1.Time
	UntilTime *metav1.Time
	TailLines *int64
	// output format arguments
	// raw is set to true when we are viewing the journal and wish to skip prefixing
	Raw    bool
	Unify  bool
	Prefix bool

	Object           runtime.Object
	GetNodeTimeout   time.Duration
	RESTClientGetter func(mapping *meta.RESTMapping) (resource.RESTClient, error)
	Builder          *resource.Builder
	LogsForObject    polymorphichelpers.LogsForObjectFunc
	ConsumeRequestFn func(rest.ResponseWrapper, corev1.ObjectReference, io.Writer) error

	genericclioptions.IOStreams
}

// NewCmdNodeLogs creates a new logs command
func NewCmdNodeLogs(factory cmdutil.Factory, streams genericclioptions.IOStreams) *cobra.Command {
	flags := NewNodeLogsCmdFlags(factory, streams)

	cmd := &cobra.Command{
		Use:                   logsNodesUsageStr,
		DisableFlagsInUseLine: true,
		Short:                 "Display and filter node logs",
		Long:                  logsNodesLong,
		Example:               logsNodesExample,
		Run: func(cmd *cobra.Command, args []string) {
			o, err := flags.ToOptions(args, cmd.Flags().Changed("boot"))
			cmdutil.CheckErr(err)
			cmdutil.CheckErr(o.Validate())
			cmdutil.CheckErr(o.Run())
		},
	}
	flags.AddFlags(cmd)
	return cmd
}

// AddFlags registers flags for a cli.
func (flags *NodeLogsCmdFlags) AddFlags(cmd *cobra.Command) {
	cmd.Flags().StringSliceVarP(&flags.Query, "query", "Q", flags.Query, "Return log entries that matches any of the specified file or service(s).")
	cmd.Flags().StringVarP(&flags.Pattern, "pattern", "p", flags.Pattern, "Filter log entries by the provided regex pattern. Only applies to service logs.")
	cmd.Flags().StringVar(&flags.SinceTime, "since-time", flags.SinceTime, "Return logs after a specific date (RFC3339). Only applies to service logs.")
	cmd.Flags().StringVar(&flags.UntilTime, "until-time", flags.UntilTime, "Return logs before a specific date (RFC3339). Only applies to service logs.")
	cmd.Flags().Int64Var(&flags.Boot, "boot", flags.Boot, "Show messages from a specific boot. Use negative numbers. "+
		"Passing invalid boot offset will fail retrieving logs. "+
		"Only applies to Linux service logs.")
	cmd.Flags().Int64Var(&flags.TailLines, "tail", flags.TailLines,
		"Return up to this many lines (not more than 100k) from the end of the log. Only applies to service logs.")
	cmd.Flags().StringVar(&flags.Role, "role", flags.Role, "Set a label selector by node role to filter on.")
	cmd.Flags().StringVarP(&flags.Selector, "selector", "l", flags.Selector, "Selector (label query) to filter on.")
	cmd.Flags().BoolVar(&flags.Raw, "raw", flags.Raw, "Perform no transformation of the returned data.")
	cmd.Flags().BoolVar(&flags.Unify, "unify", flags.Unify, "Interleave logs by sorting the output. Defaults on when viewing node journal logs.")
}

// ToOptions converts from CLI inputs to runtime inputs.
func (flags *NodeLogsCmdFlags) ToOptions(args []string, bootChanged bool) (*NodeLogsCmdOptions,
	error) {
	o := &NodeLogsCmdOptions{
		RESTClientGetter: flags.Factory.UnstructuredClientForMapping,
		Unify:            flags.Unify,
		Raw:              flags.Raw,
		LogsForObject:    polymorphichelpers.LogsForObjectFn,
		Resources:        args,
		IOStreams:        flags.IOStreams,
	}

	if bootChanged {
		o.Boot = &flags.Boot
	}

	if len(flags.Query) > 0 {
		o.Query = flags.Query
	}

	files, services := parseQuery(flags.Query)
	if files == 0 || services > 0 {
		o.Raw = true
		o.Unify = true
	}

	if len(flags.SinceTime) > 0 {
		t, err := util.ParseRFC3339(flags.SinceTime, metav1.Now)
		if err != nil {
			return nil, err
		}

		o.SinceTime = &t
	}

	if len(flags.UntilTime) > 0 {
		t, err := util.ParseRFC3339(flags.UntilTime, metav1.Now)
		if err != nil {
			return nil, err
		}

		o.UntilTime = &t
	}

	if len(flags.Pattern) > 0 {
		o.Pattern = flags.Pattern
	}

	if flags.TailLines > 0 {
		o.TailLines = &flags.TailLines
	}

	var nodeSelectorTail int64 = 10
	if len(flags.Selector) > 0 && o.TailLines == nil {
		o.TailLines = &nodeSelectorTail
	}

	builder := flags.Factory.NewBuilder().
		WithScheme(scheme.Scheme, scheme.Scheme.PrioritizedVersionsAllGroups()...).
		SingleResourceType()

	if len(o.Resources) > 0 {
		builder.ResourceNames("nodes", o.Resources...)
	}
	if len(o.Role) > 0 {
		req, err := labels.NewRequirement(fmt.Sprintf("node-role.kubernetes.io/%s", o.Role), selection.Exists, nil)
		if err != nil {
			return nil, fmt.Errorf("invalid --role: %v", err)
		}
		o.Selector = req.String()
	}
	if len(o.Selector) > 0 {
		builder.ResourceTypes("nodes").LabelSelectorParam(o.Selector)
	}
	o.Builder = builder

	// Initialize here so that NodeLogsConsumeRequest has the cmdOptions set
	o.ConsumeRequestFn = o.NodeLogsConsumeRequest

	return o, nil
}

func (o NodeLogsCmdOptions) Validate() error {
	if len(o.Resources) == 0 && len(o.Selector) == 0 {
		return fmt.Errorf("at least one node name or a selector (-l) must be specified")
	}
	if len(o.Resources) > 0 && len(o.Selector) > 0 {
		return fmt.Errorf("node names and selector may not both be specified")
	}
	if o.TailLines != nil && *o.TailLines < -1 {
		return fmt.Errorf("--tail must be greater than or equal to -1")
	}
	if o.Boot != nil && *o.Boot > 0 {
		return fmt.Errorf("--boot only accepts values less than 1")
	}

	return nil
}

// Run retrieves node logs
func (o NodeLogsCmdOptions) Run() error {
	var err error

	result := o.Builder.ContinueOnError().Flatten().Do()
	o.Prefix = o.Prefix || !result.TargetsSingleItems()
	requests := make(map[corev1.ObjectReference]rest.ResponseWrapper)
	err = result.Visit(func(info *resource.Info, err error) error {
		if err != nil {
			return nil
		}
		ref, err := reference.GetReference(scheme.Scheme, info.Object)
		if err != nil {
			return fmt.Errorf("unable to get reference to node %s: %v", info.Name, err)
		}
		mapping := info.ResourceMapping()
		client, err := o.RESTClientGetter(mapping)
		if err != nil {
			return fmt.Errorf("unable to get REST client for node %s: %v", info.Name, err)
		}
		path := client.Get().
			Namespace(info.Namespace).Name(info.Name).
			Resource(mapping.Resource.Resource).SubResource("proxy", "logs").URL().Path
		req := client.Get().RequestURI(path).
			SetHeader("Accept", "text/plain, */*").
			SetHeader("Accept-Encoding", "gzip")

		if len(o.Query) > 0 {
			for _, query := range o.Query {
				req.Param("query", query)
			}
		}
		if o.UntilTime != nil {
			req.Param("untilTime", o.UntilTime.Format(time.RFC3339))
		}
		if o.SinceTime != nil {
			req.Param("sinceTime", o.SinceTime.Format(time.RFC3339))
		}
		if o.Boot != nil {
			req.Param("boot", strconv.FormatInt(*o.Boot, 10))
		}
		if len(o.Pattern) > 0 {
			req.Param("pattern", o.Pattern)
		}
		if o.TailLines != nil && *o.TailLines > 0 {
			req.Param("tailLines", strconv.FormatInt(*o.TailLines, 10))
		}
		requests[*ref] = req
		return nil
	})
	if err != nil {
		return err
	}

	// buffer output for slightly better streaming performance
	out := bufio.NewWriterSize(o.Out, logsNodesWriterBufferSize)
	defer out.Flush()

	if o.Unify {
		// unified output is each source, interleaved in lexographic order (assumes
		// the source input is sorted by time)
		err = o.parallelConsumeRequest(requests)
	} else {
		err = o.sequentialConsumeRequest(requests)
	}

	if err, ok := err.(*apierrors.StatusError); ok && err.ErrStatus.Details != nil {
		for _, cause := range err.ErrStatus.Details.Causes {
			fmt.Fprintf(o.ErrOut, "  %s\n", cause.Message)
		}
	}
	return err
}

func (o NodeLogsCmdOptions) parallelConsumeRequest(requests map[corev1.ObjectReference]rest.ResponseWrapper) error {
	// TODO: refactor this function and dedupe code with LogOptions for pods
	o.Prefix = false
	wg := &sync.WaitGroup{}
	wg.Add(len(requests))
	var readers []Reader
	for objRef, request := range requests {
		reader, writer := io.Pipe()
		readers = append(readers, Reader{
			R: reader,
		})
		go func(objRef corev1.ObjectReference, request rest.ResponseWrapper) {
			defer wg.Done()
			if err := o.ConsumeRequestFn(request, objRef, writer); err != nil {
				writer.CloseWithError(err)
				// It's important to return here to propagate the error via the pipe
				return
			}
		}(objRef, request)

		go func() {
			wg.Wait()
			writer.Close()
		}()
	}

	_, err := NewMergeReader(readers...).WriteTo(o.Out)

	return err
}

func (o NodeLogsCmdOptions) sequentialConsumeRequest(requests map[corev1.ObjectReference]rest.ResponseWrapper) error {
	// TODO: refactor this function and dedupe code with LogOptions for pods
	for objRef, request := range requests {
		if err := o.ConsumeRequestFn(request, objRef, o.Out); err != nil {
			return err
		}
	}

	return nil
}

// NodeLogsConsumeRequest reads the data from request and writes into
// the out writer.
func (o NodeLogsCmdOptions) NodeLogsConsumeRequest(request rest.ResponseWrapper, objRef corev1.ObjectReference, out io.Writer) error {
	in, err := request.Stream(context.TODO())
	if err != nil {
		return err
	}
	defer in.Close()

	// raw output implies we may be getting binary content directly
	// from the remote and so we want to perform no translation
	if o.Raw {
		// TODO: optionallyDecompress should be implemented by checking
		// the content-encoding of the response, but we perform optional
		// decompression here in case the content of the logs on the server
		// is also gzipped.
		return optionallyDecompress(out, in)
	}

	var prefix []byte
	if o.Prefix {
		prefix = []byte(fmt.Sprintf("%s ", objRef.Name))
	}
	return outputDirectoryEntriesOrContent(out, in, prefix)
}

// parseQuery traverses the query slice and returns the number of files and services
func parseQuery(query []string) (int, int) {
	var files, services int
	for _, q := range query {
		if strings.ContainsAny(q, "/\\") {
			files++
		} else {
			services++
		}
	}
	return files, services
}

func optionallyDecompress(out io.Writer, in io.Reader) error {
	buf := bufio.NewReaderSize(in, logsNodesReaderBufferSize)
	head, err := buf.Peek(1024)
	if err != nil && err != io.EOF {
		return err
	}
	if _, err := gzip.NewReader(bytes.NewBuffer(head)); err != nil {
		// not a gzipped stream
		_, err = io.Copy(out, buf)
		return err
	}
	r, err := gzip.NewReader(buf)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, r)
	return err
}

func outputDirectoryEntriesOrContent(out io.Writer, in io.Reader, prefix []byte) error {
	buf := bufio.NewReaderSize(in, logsNodesReaderBufferSize)

	// turn href links into lines of output
	content, _ := buf.Peek(logsNodesReaderBufferSize)
	if bytes.HasPrefix(content, []byte("<pre>")) {
		reLink := regexp.MustCompile(`href="([^"]+)"`)
		s := bufio.NewScanner(buf)
		s.Split(func(data []byte, atEOF bool) (advance int, token []byte, err error) {
			matches := reLink.FindSubmatchIndex(data)
			if matches == nil {
				advance = bytes.LastIndex(data, []byte("\n"))
				if advance == -1 {
					advance = 0
				}
				return advance, nil, nil
			}
			advance = matches[1]
			token = data[matches[2]:matches[3]]
			return advance, token, nil
		})
		for s.Scan() {
			if _, err := out.Write(prefix); err != nil {
				return err
			}
			if _, err := fmt.Fprintln(out, s.Text()); err != nil {
				return err
			}
		}
		return s.Err()
	}

	// without a prefix we can copy directly
	if len(prefix) == 0 {
		_, err := io.Copy(out, buf)
		return err
	}

	r := NewMergeReader(Reader{R: buf, Prefix: prefix})
	_, err := r.WriteTo(out)
	return err
}

// Reader wraps an io.Reader and inserts the provided prefix at the
// beginning of the output and before each newline character found
// in the stream.
type Reader struct {
	R      io.Reader
	Prefix []byte
}

type mergeReader []Reader

// NewMergeReader attempts to display the provided readers as line
// oriented output in lexographic order by always reading the next
// available line from the reader with the "smallest" line.
//
// For example, given the readers with the following lines:
//
//	 1: A
//	    B
//	    D
//	 2: C
//	    D
//	    E
//
//	the reader would contain:
//	    A
//	    B
//	    C
//	    D
//	    D
//	    E
//
// The merge reader uses bufio.NewReader() for each input and the
// ReadLine() method to find the next shortest input. If a given
// line is longer than the buffer size of 4096, and all readers
// have the same initial 4096 characters, the order is undefined.
func NewMergeReader(r ...Reader) io.WriterTo {
	return mergeReader(r)
}

// WriteTo copies the provided readers into the provided output.
func (r mergeReader) WriteTo(out io.Writer) (int64, error) {
	// shortcut common cases
	switch len(r) {
	case 0:
		return 0, nil
	case 1:
		if len(r[0].Prefix) == 0 {
			return io.Copy(out, r[0].R)
		}
	}

	// initialize the buffered readers
	var buffers sortedBuffers
	var errs []error
	for _, in := range r {
		buf := &buffer{
			r:      bufio.NewReaderSize(in.R, logsNodesReaderBufferSize),
			prefix: in.Prefix,
		}
		if err := buf.next(); err != nil {
			errs = append(errs, err)
			continue
		}
		buffers = append(buffers, buf)
	}

	var n int64
	for len(buffers) > 0 {
		// find the lowest buffer
		sort.Sort(buffers)

		// write out the line from the smallest buffer
		buf := buffers[0]

		if len(buf.prefix) > 0 {
			b, err := out.Write(buf.prefix)
			n += int64(b)
			if err != nil {
				return n, err
			}
		}

		for {
			done := !buf.linePrefix
			b, err := out.Write(buf.line)
			n += int64(b)
			if err != nil {
				return n, err
			}

			// try to fill the buffer, and if we get an error reading drop this source
			if err := buf.next(); err != nil {
				errs = append(errs, err)
				buffers = buffers[1:]
				break
			}

			// we reached the end of our line
			if done {
				break
			}
		}
		b, err := fmt.Fprintln(out)
		n += int64(b)
		if err != nil {
			return n, err
		}
	}

	return n, errors.FilterOut(errors.NewAggregate(errs), func(err error) bool { return err == io.EOF })
}

type buffer struct {
	r          *bufio.Reader
	prefix     []byte
	line       []byte
	linePrefix bool
}

func (b *buffer) next() error {
	var err error
	b.line, b.linePrefix, err = b.r.ReadLine()
	return err
}

type sortedBuffers []*buffer

func (buffers sortedBuffers) Less(i, j int) bool {
	return bytes.Compare(buffers[i].line, buffers[j].line) < 0
}
func (buffers sortedBuffers) Swap(i, j int) {
	buffers[i], buffers[j] = buffers[j], buffers[i]
}
func (buffers sortedBuffers) Len() int {
	return len(buffers)
}
