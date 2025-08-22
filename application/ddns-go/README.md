# ddns-go

## introduction

ddns-go is a dynamic DNS tool that automatically updates DNS records when your IP address changes. It supports various DNS providers and offers a user-friendly web interface for configuration and management.

## resources

1. helm chart
    * [ddns-go](chart/)

2. install with helm
    * ```shell
      helm upgrade --install ddns-go oci://ghcr.io/ben-wangz/k8s-at-home-charts/ddns-go \
        --atomic \
        --version 1.0.0 \
        --namespace basic-components \
        --create-namespace \
        --set image.repository=m.daocloud.io/docker.io/jeessy/ddns-go \
        --set ingress.enabled=true \
        --set ingress.hostname=ddns-go.example.com
      ```