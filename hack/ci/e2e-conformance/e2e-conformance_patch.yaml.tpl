apiVersion: infrastructure.cluster.x-k8s.io/v1alpha4
kind: OpenStackCluster
metadata:
  name: ${CLUSTER_NAME}
  namespace: ${CLUSTER_NAME}
spec:
  bastion:
    enabled: true
    instance:
      flavor: ${OPENSTACK_BASTION_MACHINE_FLAVOR}
      image: ${OPENSTACK_BASTION_IMAGE_NAME}
      sshKeyName: ${OPENSTACK_SSH_KEY_NAME}
---
apiVersion: controlplane.cluster.x-k8s.io/v1alpha4
kind: KubeadmControlPlane
metadata:
  name: "${CLUSTER_NAME}-control-plane"
  namespace: ${CLUSTER_NAME}
spec:
  kubeadmConfigSpec:
    clusterConfiguration:
      imageRepository: ${IMAGE_REPOSITORY}
    initConfiguration:
      nodeRegistration:
        kubeletExtraArgs:
          v: "8"
    joinConfiguration:
      nodeRegistration:
        kubeletExtraArgs:
          v: "8"
    verbosity: 8
    preKubeadmCommands:
    - bash -c /tmp/kubeadm-bootstrap.sh
    files:
    - path: /etc/kubernetes/cloud.conf
      owner: root
      permissions: "0600"
      content: ${OPENSTACK_CLOUD_PROVIDER_CONF_B64}
      encoding: base64
    - path: /etc/certs/cacert
      owner: root
      permissions: "0600"
      content: ${OPENSTACK_CLOUD_CACERT_B64}
      encoding: base64
    - path: /tmp/kubeadm-bootstrap.sh
      owner: "root:root"
      permissions: "0744"
      content: |
        #!/bin/bash

        set -o nounset
        set -o pipefail
        set -o errexit
        set -e

        [[ ${USE_CI_ARTIFACTS} == "false" ]] && exit 0

        [[ $(id -u) != 0 ]] && SUDO="sudo" || SUDO=""

        # This script installs kubectl, kubelet, and kubeadm binaries.
        LINE_SEPARATOR="*************************************************"
        echo "$LINE_SEPARATOR"

        K8S_DIR="/tmp/k8s"
        mkdir -p ${K8S_DIR}
        K8S_URL="https://storage.googleapis.com/kubernetes-release/release/${KUBERNETES_VERSION}/kubernetes-server-linux-amd64.tar.gz"
        cd ${K8S_DIR}
        wget -q ${K8S_URL}
        tar zxf kubernetes-server-linux-amd64.tar.gz
        K8S_SERVER_BIN_DIR="${K8S_DIR}/kubernetes/server/bin"

        declare -a BINARIES_TO_TEST=("kubectl" "kubelet" "kubeadm")
        for BINARY in "${BINARIES_TO_TEST[@]}"; do
          # move old binary away to avoid err "Text file busy"
          ${SUDO} mv /usr/bin/${BINARY} /usr/bin/${BINARY}.bak
          ${SUDO} cp ${K8S_SERVER_BIN_DIR}/${BINARY} /usr/bin/${BINARY}
          ${SUDO} chmod +x /usr/bin/${BINARY}
        done

        echo "$(date): checking binary versions"
        echo "ctr version: " $(ctr version)
        echo "kubeadm version: " $(kubeadm version -o=short)
        echo "kubectl version: " $(kubectl version --client=true --short=true)
        echo "kubelet version: " $(kubelet --version)

        echo "$LINE_SEPARATOR"
    users:
    - name: "capi"
      sudo: "ALL=(ALL) NOPASSWD:ALL"
      # user: capi, passwd: capi
      passwd: "$6$rounds=4096$yKTFKL6RmN128$a7cGMiNjeTSd091s6QzZcUNrMTgm3HhML5rVmpDFlCfgD7scTW7ZHr0OChcXCaeiO/kbhdn0XzIzWk63nSqRH1"
      lockPassword: false
      sshAuthorizedKeys:
      - "${OPENSTACK_SSH_KEY_PUBLIC}"
---
apiVersion: bootstrap.cluster.x-k8s.io/v1alpha4
kind: KubeadmConfigTemplate
metadata:
  name: ${CLUSTER_NAME}-md-0
  namespace: ${CLUSTER_NAME}
spec:
  template:
    spec:
      verbosity: 8
      preKubeadmCommands:
      - bash -c /tmp/kubeadm-bootstrap.sh
      files:
      - content: ${OPENSTACK_CLOUD_PROVIDER_CONF_B64}
        encoding: base64
        owner: root
        path: /etc/kubernetes/cloud.conf
        permissions: "0600"
      - content: ${OPENSTACK_CLOUD_CACERT_B64}
        encoding: base64
        owner: root
        path: /etc/certs/cacert
        permissions: "0600"
      - path: /tmp/kubeadm-bootstrap.sh
        owner: "root:root"
        permissions: "0744"
        content: |
          #!/bin/bash

          set -o nounset
          set -o pipefail
          set -o errexit
          set -e

          [[ ${USE_CI_ARTIFACTS} == "false" ]] && exit 0

          [[ $(id -u) != 0 ]] && SUDO="sudo" || SUDO=""

          # This script installs kubectl, kubelet, and kubeadm binaries.
          LINE_SEPARATOR="*************************************************"
          echo "$LINE_SEPARATOR"

          K8S_DIR=/tmp/k8s
          mkdir -p $K8S_DIR
          K8S_URL="https://dl.k8s.io/${KUBERNETES_VERSION}/kubernetes-server-linux-amd64.tar.gz"
          cd ${K8S_DIR}
          wget -q ${K8S_URL}
          tar zxvf kubernetes-server-linux-amd64.tar.gz
          K8S_SERVER_BIN_DIR="${K8S_DIR}/kubernetes/server/bin"

          declare -a BINARIES_TO_TEST=("kubectl" "kubelet" "kubeadm")
          for BINARY in "${BINARIES_TO_TEST[@]}"; do
            # move old binary away to avoid err "Text file busy"
            ${SUDO} mv /usr/bin/${BINARY} /usr/bin/${BINARY}.bak
            ${SUDO} cp ${K8S_SERVER_BIN_DIR}/${BINARY} /usr/bin/${BINARY}
            ${SUDO} chmod +x /usr/bin/${BINARY}
          done

          echo "$(date): checking binary versions"
          echo "ctr version: " $(ctr version)
          echo "kubeadm version: " $(kubeadm version -o=short)
          echo "kubectl version: " $(kubectl version --client=true --short=true)
          echo "kubelet version: " $(kubelet --version)

          echo "$LINE_SEPARATOR"
      users:
      - name: "capi"
        sudo: "ALL=(ALL) NOPASSWD:ALL"
        # user: capi, passwd: capi
        passwd: "$6$rounds=4096$yKTFKL6RmN128$a7cGMiNjeTSd091s6QzZcUNrMTgm3HhML5rVmpDFlCfgD7scTW7ZHr0OChcXCaeiO/kbhdn0XzIzWk63nSqRH1"
        lockPassword: false
        sshAuthorizedKeys:
        - "${OPENSTACK_SSH_KEY_PUBLIC}"