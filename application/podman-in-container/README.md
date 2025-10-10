# podman-in-container

## introduction

Podman-in-container provides a containerized Podman runtime with SSH access support, allowing you to run containers within Kubernetes pods. This is useful for CI/CD pipelines, development environments, or any scenario where you need to run containers dynamically within your Kubernetes cluster.

## resources

1. helm chart
    * [podman-in-container](chart/)

2. install with helm
    * Generate SSH key pair first:
      ```shell
      # Generate SSH key pair if you don't have one
      ssh-keygen -t ed25519 -f ~/.ssh/podman-in-container -N ""

      # Read the public key
      export SSH_PUBLIC_KEY=$(cat ~/.ssh/podman-in-container.pub)
      ```

    * Install with inline SSH public key:
      ```shell
      helm upgrade --install podman-in-container oci://ghcr.io/ben-wangz/k8s-at-home-charts/podman-in-container \
        --atomic \
        --version 1.1.0 \
        --namespace basic-components \
        --create-namespace \
        --set service.type=NodePort \
        --set "ssh.authorizedKeys[0]=${SSH_PUBLIC_KEY}"
      ```

    * Or create a secret with SSH authorized keys first:
      ```shell
      # Create SSH secret with your public key
      kubectl -n basic-components create secret generic podman-ssh-keys \
        --from-file=authorized_keys=~/.ssh/podman-in-container.pub

      # Install with existing secret
      helm upgrade --install podman-in-container oci://ghcr.io/ben-wangz/k8s-at-home-charts/podman-in-container \
        --atomic \
        --version 1.1.0 \
        --namespace basic-components \
        --create-namespace \
        --set service.type=NodePort \
        --set ssh.existingSecret=podman-ssh-keys
      ```

3. connect via SSH
    * Get the NodePort:
      ```shell
      export NODE_PORT=$(kubectl -n basic-components get svc podman-in-container -o jsonpath='{.spec.ports[0].nodePort}')
      export NODE_IP=$(kubectl get nodes -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}')

      echo "SSH connection: ssh -i ~/.ssh/podman-in-container -p ${NODE_PORT} root@${NODE_IP}"
      ```

    * Connect to the container:
      ```shell
      ssh -i ~/.ssh/podman-in-container -p ${NODE_PORT} root@${NODE_IP}
      ```

    * Once connected, you can use podman commands:
      ```shell
      # Pull and run a container
      podman pull alpine:latest
      podman run --rm alpine:latest echo "Hello from podman-in-container!"

      # List containers
      podman ps -a

      # List images
      podman images
      ```

4. persistence
    * The chart creates two persistent volumes:
        - `container-volume`: Stores Podman container images and data (default: 10Gi at `/var/lib/containers`)
        - `home-volume`: Stores root home directory including bash history (default: 5Gi at `/root`)

    * Configure persistence:
      ```shell
      helm upgrade --install podman-in-container oci://ghcr.io/ben-wangz/k8s-at-home-charts/podman-in-container \
        --atomic \
        --version 1.1.0 \
        --namespace basic-components \
        --create-namespace \
        --set persistence.container.size=20Gi \
        --set persistence.home.size=10Gi \
        --set persistence.container.storageClass=fast-ssd
      ```

## notes

- The container runs in privileged mode to allow Podman to function properly
- SSH is the primary access method (no ingress support for SSH protocol)
- All container images and data persist across pod restarts via PersistentVolumeClaims
- For production use, consider using LoadBalancer service type or port forwarding instead of NodePort
