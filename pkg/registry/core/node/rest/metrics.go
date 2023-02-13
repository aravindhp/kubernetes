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

package node

import (
	"sync"

	"k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/legacyregistry"
)

const (
	namespace = "kube_apiserver"
	subsystem = "node_logs"
)

var (
	// nodeLogsTLSFailure counts how many attempts to get pod logs fail on tls verification
	nodeLogsTLSFailure = metrics.NewCounter(
		&metrics.CounterOpts{
			Namespace:      namespace,
			Subsystem:      subsystem,
			Name:           "node_logs_backend_tls_failure_total",
			Help:           "Total number of requests for nodes/logs that failed due to kubelet server TLS verification",
			StabilityLevel: metrics.ALPHA,
		},
	)
)

var registerMetricsOnce sync.Once

func registerMetrics() {
	registerMetricsOnce.Do(func() {
		legacyregistry.MustRegister(nodeLogsTLSFailure)
	})
}
