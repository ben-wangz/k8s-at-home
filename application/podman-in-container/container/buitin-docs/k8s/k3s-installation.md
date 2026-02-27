# K3s Installation Guide

## K3s

Lightweight Kubernetes distribution for container environments. Includes kubectl automatically.

* installation
    + ```bash
      curl -sfL https://rancher-mirror.rancher.cn/k3s/k3s-install.sh | \
        INSTALL_K3S_MIRROR=cn \
        INSTALL_K3S_SKIP_START=true \
        INSTALL_K3S_SKIP_ENABLE=true \
        sh -
      ```

* start k3s server (no systemd in container)
    + Run in background:
      ```bash
      nohup k3s server \
        --disable=traefik \
        --disable=servicelb \
        --snapshotter=native \
        --write-kubeconfig-mode=644 \
        > /var/log/k3s.log 2>&1 &
      ```
    + Save PID for management:
      ```bash
      echo $! > /var/run/k3s.pid
      ```
    + Wait for k3s to be ready:
      ```bash
      for i in {1..60}; do
        if kubectl get nodes &>/dev/null; then
          echo "k3s is ready"
          break
        fi
        sleep 2
      done
      ```

* verify installation
    + ```bash
      kubectl get nodes
      ```
    + ```bash
      kubectl get pods -A
      ```

* configure kubectl access
    + Option 1: Use k3s config directly (requires KUBECONFIG env var):
      ```bash
      export KUBECONFIG="/etc/rancher/k3s/k3s.yaml"
      ```
    + Option 2: Copy to default location (no env var needed):
      ```bash
      mkdir -p $HOME/.kube
      cp /etc/rancher/k3s/k3s.yaml $HOME/.kube/config
      chown $(id -u):$(id -g) $HOME/.kube/config
      ```

* stop k3s server
    + ```bash
      kill $(cat /var/run/k3s.pid)
      rm -f /var/run/k3s.pid
      ```

* check status
    + ```bash
      if [ -f /var/run/k3s.pid ] && kill -0 $(cat /var/run/k3s.pid) 2>/dev/null; then
        echo "k3s is running"
      else
        echo "k3s is not running"
      fi
      ```

* one-click management script (auto-installs if not present)
    + ```bash
      k3s.sh start
      ```
    + ```bash
      k3s.sh stop
      ```
    + ```bash
      k3s.sh status
      ```
    + ```bash
      k3s.sh delete
      ```

* uninstall
    + ```bash
      /usr/local/bin/k3s-uninstall.sh
      ```

## Notes

* K3s requires privileged container mode or specific capabilities
* **kubectl included**: k3s installation automatically creates `/usr/local/bin/kubectl` symlink, no separate installation needed
* **No systemd**: Container environments typically don't have systemd, so k3s must be started manually
* Use `INSTALL_K3S_SKIP_START=true` and `INSTALL_K3S_SKIP_ENABLE=true` during installation
* Use `INSTALL_K3S_MIRROR=cn` for faster downloads in China
* **Important**: Installation parameters must be specified when starting k3s server, not during installation
* K3s server runs on port 6443 by default
* **Overlay FS issue**: Use `--snapshotter=native` to avoid overlay-over-overlay issues in container environments
* Save the PID to `/var/run/k3s.pid` for easier process management
* K3s logs are written to `/var/log/k3s.log`
* Default kubeconfig location: `/etc/rancher/k3s/k3s.yaml`
