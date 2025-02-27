#!/usr/bin/env bash

set -o errexit
set -o nounset
set -o pipefail

# This script runs e2e test against on karmada control plane.
# You should prepare your environment in advance and following environment may be you need to set or use default one.
# - CONTROL_PLANE_KUBECONFIG: absolute path of control plane KUBECONFIG file.
#
# Usage: hack/run-e2e.sh
# Example 1: hack/run-e2e.sh (run e2e with default config)
# Example 2: export KARMADA_APISERVER_KUBECONFIG=<KUBECONFIG PATH> hack/run-e2e.sh (run e2e with your KUBECONFIG)

KUBECONFIG_PATH=${KUBECONFIG_PATH:-"${HOME}/.kube"}
KARMADA_APISERVER_KUBECONFIG=${KARMADA_APISERVER_KUBECONFIG:-"/var/run/karmada/karmada-apiserver.config"}
PULL_BASED_CLUSTERS=${PULL_BASED_CLUSTERS:-"member3:$KUBECONFIG_PATH/member3.config"}

# Install ginkgo
GO111MODULE=on go install github.com/onsi/ginkgo/ginkgo

# Run e2e
export KUBECONFIG=${KARMADA_APISERVER_KUBECONFIG}
export PULL_BASED_CLUSTERS=${PULL_BASED_CLUSTERS}

ginkgo -v -race -failFast ./test/e2e/

