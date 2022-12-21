/*
Copyright 2014 The Kubernetes Authors.

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

package node

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apiserver/pkg/registry/generic"
	pkgstorage "k8s.io/apiserver/pkg/storage"
	"k8s.io/apiserver/pkg/storage/names"
	"k8s.io/kubernetes/pkg/api/legacyscheme"
	api "k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/kubernetes/pkg/apis/core/validation"
	"k8s.io/kubernetes/pkg/kubelet/client"
	proxyutil "k8s.io/kubernetes/pkg/proxy/util"
	"sigs.k8s.io/structured-merge-diff/v4/fieldpath"
)

// nodeStrategy implements behavior for nodes
type nodeStrategy struct {
	runtime.ObjectTyper
	names.NameGenerator
}

// Nodes is the default logic that applies when creating and updating Node
// objects.
var Strategy = nodeStrategy{legacyscheme.Scheme, names.SimpleNameGenerator}

// NamespaceScoped is false for nodes.
func (nodeStrategy) NamespaceScoped() bool {
	return false
}

// GetResetFields returns the set of fields that get reset by the strategy
// and should not be modified by the user.
func (nodeStrategy) GetResetFields() map[fieldpath.APIVersion]*fieldpath.Set {
	fields := map[fieldpath.APIVersion]*fieldpath.Set{
		"v1": fieldpath.NewSet(
			fieldpath.MakePathOrDie("status"),
		),
	}

	return fields
}

// AllowCreateOnUpdate is false for nodes.
func (nodeStrategy) AllowCreateOnUpdate() bool {
	return false
}

// PrepareForCreate clears fields that are not allowed to be set by end users on creation.
func (nodeStrategy) PrepareForCreate(ctx context.Context, obj runtime.Object) {
	node := obj.(*api.Node)
	dropDisabledFields(node, nil)
}

// PrepareForUpdate clears fields that are not allowed to be set by end users on update.
func (nodeStrategy) PrepareForUpdate(ctx context.Context, obj, old runtime.Object) {
	newNode := obj.(*api.Node)
	oldNode := old.(*api.Node)
	newNode.Status = oldNode.Status

	dropDisabledFields(newNode, oldNode)
}

func dropDisabledFields(node *api.Node, oldNode *api.Node) {
	// Nodes allow *all* fields, including status, to be set on create.
	// for create
	if oldNode == nil {
		node.Spec.ConfigSource = nil
		node.Status.Config = nil
	}

	// for update
	if !nodeConfigSourceInUse(oldNode) && oldNode != nil {
		node.Spec.ConfigSource = nil
	}

}

// nodeConfigSourceInUse returns true if node's Spec ConfigSource is set(used)
func nodeConfigSourceInUse(node *api.Node) bool {
	if node == nil {
		return false
	}
	if node.Spec.ConfigSource != nil {
		return true
	}
	return false
}

// Validate validates a new node.
func (nodeStrategy) Validate(ctx context.Context, obj runtime.Object) field.ErrorList {
	node := obj.(*api.Node)
	return validation.ValidateNode(node)
}

// WarningsOnCreate returns warnings for the creation of the given object.
func (nodeStrategy) WarningsOnCreate(ctx context.Context, obj runtime.Object) []string {
	return dynamicKubeletConfigIsDeprecatedWarning(obj)
}

// Canonicalize normalizes the object after validation.
func (nodeStrategy) Canonicalize(obj runtime.Object) {
}

// ValidateUpdate is the default update validation for an end user.
func (nodeStrategy) ValidateUpdate(ctx context.Context, obj, old runtime.Object) field.ErrorList {
	errorList := validation.ValidateNode(obj.(*api.Node))
	return append(errorList, validation.ValidateNodeUpdate(obj.(*api.Node), old.(*api.Node))...)
}

// WarningsOnUpdate returns warnings for the given update.
func (nodeStrategy) WarningsOnUpdate(ctx context.Context, obj, old runtime.Object) []string {
	return dynamicKubeletConfigIsDeprecatedWarning(obj)
}

func (nodeStrategy) AllowUnconditionalUpdate() bool {
	return true
}

type nodeStatusStrategy struct {
	nodeStrategy
}

var StatusStrategy = nodeStatusStrategy{Strategy}

// GetResetFields returns the set of fields that get reset by the strategy
// and should not be modified by the user.
func (nodeStatusStrategy) GetResetFields() map[fieldpath.APIVersion]*fieldpath.Set {
	fields := map[fieldpath.APIVersion]*fieldpath.Set{
		"v1": fieldpath.NewSet(
			fieldpath.MakePathOrDie("spec"),
		),
	}

	return fields
}

func (nodeStatusStrategy) PrepareForUpdate(ctx context.Context, obj, old runtime.Object) {
	newNode := obj.(*api.Node)
	oldNode := old.(*api.Node)
	newNode.Spec = oldNode.Spec

	if !nodeStatusConfigInUse(oldNode) {
		newNode.Status.Config = nil
	}
}

// nodeStatusConfigInUse returns true if node's Status Config is set(used)
func nodeStatusConfigInUse(node *api.Node) bool {
	if node == nil {
		return false
	}
	if node.Status.Config != nil {
		return true
	}
	return false
}

func (nodeStatusStrategy) ValidateUpdate(ctx context.Context, obj, old runtime.Object) field.ErrorList {
	return validation.ValidateNodeUpdate(obj.(*api.Node), old.(*api.Node))
}

// WarningsOnUpdate returns warnings for the given update.
func (nodeStatusStrategy) WarningsOnUpdate(ctx context.Context, obj, old runtime.Object) []string {
	return nil
}

// Canonicalize normalizes the object after validation.
func (nodeStatusStrategy) Canonicalize(obj runtime.Object) {
}

// ResourceGetter is an interface for retrieving resources by ResourceLocation.
type ResourceGetter interface {
	Get(context.Context, string, *metav1.GetOptions) (runtime.Object, error)
}

// NodeToSelectableFields returns a field set that represents the object.
func NodeToSelectableFields(node *api.Node) fields.Set {
	objectMetaFieldsSet := generic.ObjectMetaFieldsSet(&node.ObjectMeta, false)
	specificFieldsSet := fields.Set{
		"spec.unschedulable": fmt.Sprint(node.Spec.Unschedulable),
	}
	return generic.MergeFieldsSets(objectMetaFieldsSet, specificFieldsSet)
}

// GetAttrs returns labels and fields of a given object for filtering purposes.
func GetAttrs(obj runtime.Object) (labels.Set, fields.Set, error) {
	nodeObj, ok := obj.(*api.Node)
	if !ok {
		return nil, nil, fmt.Errorf("not a node")
	}
	return labels.Set(nodeObj.ObjectMeta.Labels), NodeToSelectableFields(nodeObj), nil
}

// MatchNode returns a generic matcher for a given label and field selector.
func MatchNode(label labels.Selector, field fields.Selector) pkgstorage.SelectionPredicate {
	return pkgstorage.SelectionPredicate{
		Label:       label,
		Field:       field,
		GetAttrs:    GetAttrs,
		IndexFields: []string{"metadata.name"},
	}
}

// NameTriggerFunc returns value metadata.namespace of given object.
func NameTriggerFunc(obj runtime.Object) string {
	return obj.(*api.Node).ObjectMeta.Name
}

// ResourceLocation returns a URL and transport which one can use to send traffic for the specified node.
func ResourceLocation(getter ResourceGetter, connection client.ConnectionInfoGetter, proxyTransport http.RoundTripper, ctx context.Context, id string) (*url.URL, http.RoundTripper, error) {
	schemeReq, name, portReq, valid := utilnet.SplitSchemeNamePort(id)
	if !valid {
		return nil, nil, errors.NewBadRequest(fmt.Sprintf("invalid node request %q", id))
	}

	info, err := connection.GetConnectionInfo(ctx, types.NodeName(name))
	if err != nil {
		return nil, nil, err
	}

	if err := proxyutil.IsProxyableHostname(ctx, &net.Resolver{}, info.Hostname); err != nil {
		return nil, nil, errors.NewBadRequest(err.Error())
	}

	// We check if we want to get a default Kubelet's transport. It happens if either:
	// - no port is specified in request (Kubelet's port is default)
	// - the requested port matches the kubelet port for this node
	if portReq == "" || portReq == info.Port {
		return &url.URL{
				Scheme: info.Scheme,
				Host:   net.JoinHostPort(info.Hostname, info.Port),
			},
			info.Transport,
			nil
	}

	// Otherwise, return the requested scheme and port, and the proxy transport
	return &url.URL{Scheme: schemeReq, Host: net.JoinHostPort(info.Hostname, portReq)}, proxyTransport, nil
}

func dynamicKubeletConfigIsDeprecatedWarning(obj runtime.Object) []string {
	newNode := obj.(*api.Node)
	if newNode.Spec.ConfigSource != nil {
		var warnings []string
		// KEP https://github.com/kubernetes/enhancements/issues/281
		warnings = append(warnings, "spec.configSource: the feature is removed")
		return warnings
	}
	return nil
}

func getNode(ctx context.Context, getter ResourceGetter, name string) (*api.Node, error) {
	obj, err := getter.Get(ctx, name, &metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	node := obj.(*api.Node)
	if node == nil {
		return nil, fmt.Errorf("Unexpected object type: %#v", node)
	}
	return node, nil
}

// LogQueryLocation returns the node log query URL for a node
func LogQueryLocation(
	ctx context.Context, getter ResourceGetter,
	connInfo client.ConnectionInfoGetter,
	name string,
	opts *api.NodeLogQueryOptions,
) (*url.URL, http.RoundTripper, error) {
	node, err := getNode(ctx, getter, name)
	if err != nil {
		return nil, nil, err
	}

	nodeInfo, err := connInfo.GetConnectionInfo(ctx, types.NodeName(node.ObjectMeta.Name))
	if err != nil {
		return nil, nil, err
	}
	params := url.Values{}
	path := fmt.Sprintf("/logs/%s", node.Name)
	if opts.SinceTime != nil {
		params.Add("sinceTime", opts.SinceTime.Format(time.RFC3339))
	}
	if opts.UntilTime != nil {
		params.Add("untilTime", opts.UntilTime.Format(time.RFC3339))
	}
	if opts.TailLines != nil {
		params.Add("tailLines", strconv.FormatInt(*opts.TailLines, 10))
	}
	if opts.Pattern != "" {
		params.Add("pattern", opts.Pattern)
	}
	if opts.Boot != nil {
		params.Add("boot", strconv.FormatInt(*opts.Boot, 10))
	}
	if len(opts.Query) > 0 {
		for _, query := range opts.Query {
			params.Add("query", query)
		}
	}
	loc := &url.URL{
		Scheme:   nodeInfo.Scheme,
		Host:     net.JoinHostPort(nodeInfo.Hostname, nodeInfo.Port),
		Path:     path,
		RawQuery: params.Encode(),
	}
	return loc, nodeInfo.Transport, nil
}
