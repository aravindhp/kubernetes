#!/bin/bash

set -o nounset
set -o errexit
set -o pipefail

# This script is executes kubernetes e2e tests against an openshift
# cluster. It is intended to be copied to the kubernetes-tests image
# for use in CI and should have no dependencies beyond oc, kubectl and
# k8s-e2e.test.

function setup_bastion() {
    cluster_profile=/var/run/secrets/ci.openshift.io/cluster-profile
    GCE_SSH_KEY=${cluster_profile}/ssh-privatekey
    KUBE_SSH_KEY_PATH=${cluster_profile}/ssh-privatekey
    KUBE_SSH_USER=core
    SSH_BASTION_NAMESPACE=test-ssh-bastion
    export GCE_SSH_KEY KUBE_SSH_USER KUBE_SSH_KEY_PATH SSH_BASTION_NAMESPACE
    echo "Setting up ssh bastion"

    # configure the local container environment to have the correct SSH configuration
    mkdir -p ~/.ssh
    cp "${KUBE_SSH_KEY_PATH}" ~/.ssh/id_rsa
    chmod 0600 ~/.ssh/id_rsa
    if ! whoami &> /dev/null; then
        if [[ -w /etc/passwd ]]; then
            echo "${USER_NAME:-default}:x:$(id -u):0:${USER_NAME:-default} user:${HOME}:/sbin/nologin" >> /etc/passwd
        fi
    fi

    # if this is run from a flow that does not have the ssh-bastion step, deploy the bastion
    if ! oc get -n "${SSH_BASTION_NAMESPACE}" ssh-bastion; then
        curl https://raw.githubusercontent.com/eparis/ssh-bastion/master/deploy/deploy.sh | bash -x
    fi

    # locate the bastion host for use within the tests
    for _ in $(seq 0 30); do
        # AWS fills only .hostname of a service
        BASTION_HOST=$(oc get service -n "${SSH_BASTION_NAMESPACE}" ssh-bastion -o jsonpath='{.status.loadBalancer.ingress[0].hostname}')
        if [[ -n "${BASTION_HOST}" ]]; then break; fi
        # Azure fills only .ip of a service. Use it as bastion host.
        BASTION_HOST=$(oc get service -n "${SSH_BASTION_NAMESPACE}" ssh-bastion -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
        if [[ -n "${BASTION_HOST}" ]]; then break; fi
        echo "Waiting for SSH bastion load balancer service"
        sleep 10
    done
    if [[ -z "${BASTION_HOST}" ]]; then
        echo >&2 "Failed to find bastion address, exiting"
        exit 1
    fi
    export KUBE_SSH_BASTION="${BASTION_HOST}:22"
}

# Identify the platform under test to allow skipping tests that are
# not compatible.
CLUSTER_TYPE="${CLUSTER_TYPE:-gcp}"
case "${CLUSTER_TYPE}" in
  gcp)
    # gce is used as a platform label instead of gcp
    PLATFORM=gce
    # Bastion is required for running the NodeLogQuery tests
    setup_bastion
    ;;
  *)
    PLATFORM="${CLUSTER_TYPE}"
    ;;
esac

# openshift-tests will check the cluster's network configuration and
# automatically skip any incompatible tests. We have to do that manually
# here.
NETWORK_SKIPS="\[Skipped:Network/OpenShiftSDN\]|\[Feature:Networking-IPv6\]|\[Feature:IPv6DualStack.*\]|\[Feature:SCTPConnectivity\]"

# Support serial and parallel test suites
TEST_SUITE="${TEST_SUITE:-parallel}"
COMMON_SKIPS="\[Slow\]|\[Disruptive\]|\[Flaky\]|\[Disabled:.+\]|\[Skipped:${PLATFORM}\]|${NETWORK_SKIPS}"
case "${TEST_SUITE}" in
serial)
  DEFAULT_TEST_ARGS="-focus=\[Serial\] -skip=${COMMON_SKIPS}"
  NODES=1
  ;;
parallel)
  DEFAULT_TEST_ARGS="-skip=\[Serial\]|${COMMON_SKIPS}"
  # Use the same number of nodes - 30 - as specified for the parallel
  # suite defined in origin.
  NODES=${NODES:-30}
  ;;
*)
  echo >&2 "Unsupported test suite '${TEST_SUITE}'"
  exit 1
  ;;
esac

# Set KUBE_E2E_TEST_ARGS to configure test arguments like
# -skip and -focus.
KUBE_E2E_TEST_ARGS="${KUBE_E2E_TEST_ARGS:-${DEFAULT_TEST_ARGS}}"

# k8s-e2e.test and ginkgo are expected to be in the path in
# CI. Outside of CI, ensure k8s-e2e.test and ginkgo are built and
# available in PATH.
if ! which k8s-e2e.test &> /dev/null; then
  make WHAT=vendor/github.com/onsi/ginkgo/v2/ginkgo
  make WHAT=openshift-hack/e2e/k8s-e2e.test
  ROOT_PATH="$(cd "$(dirname "${BASH_SOURCE[0]}")/.."; pwd -P)"
  PATH="${ROOT_PATH}/_output/local/bin/$(go env GOHOSTOS)/$(go env GOARCH):${PATH}"
  export PATH
fi

# Execute OpenShift prerequisites
# Disable container security
oc adm policy add-scc-to-group privileged system:authenticated system:serviceaccounts
oc adm policy add-scc-to-group anyuid system:authenticated system:serviceaccounts
unschedulable="$( ( oc get nodes -o name -l 'node-role.kubernetes.io/master'; ) | wc -l )"

test_report_dir="${ARTIFACTS:-/tmp/artifacts}"
mkdir -p "${test_report_dir}"

# Retrieve the hostname of the server to enable kubectl testing
SERVER=
SERVER="$( kubectl config view | grep server | head -n 1 | awk '{print $2}' )"

# shellcheck disable=SC2086
ginkgo \
  --flake-attempts=3 \
  --timeout="24h" \
  --output-interceptor-mode=none \
  -nodes "${NODES}" -no-color ${KUBE_E2E_TEST_ARGS} \
  "$( which k8s-e2e.test )" -- \
  -report-dir "${test_report_dir}" \
  -host "${SERVER}" \
  -allowed-not-ready-nodes ${unschedulable} \
  2>&1 | tee -a "${test_report_dir}/k8s-e2e.log"
