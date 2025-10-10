# yacd

## introduction

yacd (Yet Another Clash Dashboard) is a web-based dashboard for Clash, a powerful proxy tool. It provides an intuitive interface to manage and monitor your Clash instances.

## resources

1. helm chart
    * [yacd](chart/)

2. install with helm
    * ```shell
      # Get the latest chart version
      export CHART_VERSION=$(bash ../../tools/get-version.sh yacd chart)

      helm upgrade --install yacd oci://ghcr.io/ben-wangz/k8s-at-home-charts/yacd \
        --atomic \
        --version ${CHART_VERSION} \
        --namespace basic-components \
        --create-namespace \
        --set image.repository=m.daocloud.io/docker.io/haishanh/yacd \
        --set ingress.enabled=true \
        --set ingress.hostname=yacd.example.com
      ```
