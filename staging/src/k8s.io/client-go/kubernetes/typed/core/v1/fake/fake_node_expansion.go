/*
Copyright 2016 The Kubernetes Authors.

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

package fake

import (
	"context"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	restclient "k8s.io/client-go/rest"
	fakerest "k8s.io/client-go/rest/fake"
	core "k8s.io/client-go/testing"
)

// TODO: Should take a PatchType as an argument probably.
func (c *FakeNodes) PatchStatus(_ context.Context, nodeName string, data []byte) (*v1.Node, error) {
	// TODO: Should be configurable to support additional patch strategies.
	pt := types.StrategicMergePatchType
	obj, err := c.Fake.Invokes(
		core.NewRootPatchSubresourceAction(nodesResource, nodeName, pt, data, "status"), &v1.Node{})
	if obj == nil {
		return nil, err
	}

	return obj.(*v1.Node), err
}

func (c *FakeNodes) GetLogs(nodeName string, opts *v1.NodeLogQueryOptions) *restclient.Request {
	action := core.GenericActionImpl{}
	action.Verb = "get"
	action.Resource = nodesResource
	action.Subresource = "log"
	action.Value = opts

	_, _ = c.Fake.Invokes(action, &v1.Node{})
	fakeClient := &fakerest.RESTClient{
		Client: fakerest.CreateHTTPClient(func(request *http.Request) (*http.Response, error) {
			resp := &http.Response{
				StatusCode: http.StatusOK,
				Body:       ioutil.NopCloser(strings.NewReader("fake node logs")),
			}
			return resp, nil
		}),
		NegotiatedSerializer: scheme.Codecs.WithoutConversion(),
		GroupVersion:         nodesKind.GroupVersion(),
		VersionedAPIPath:     fmt.Sprintf("/api/v1/nodes/%s/log", nodeName),
	}
	return fakeClient.Request()
}
