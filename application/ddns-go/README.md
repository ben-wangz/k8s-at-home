# ddns-go

## introduction

ddns-go is a dynamic DNS tool that automatically updates DNS records when your IP address changes. It supports various DNS providers and offers a user-friendly web interface for configuration and management.

## resources

1. helm chart
    * [ddns-go](chart/)

2. install with helm
    * ```shell
      # Get the latest chart version
      export CHART_VERSION=$(bash ../../tools/get-version.sh ddns-go chart)

      helm upgrade --install ddns-go oci://ghcr.io/ben-wangz/k8s-at-home-charts/ddns-go \
        --atomic \
        --version ${CHART_VERSION} \
        --namespace basic-components \
        --create-namespace \
        --set image.repository=m.daocloud.io/docker.io/jeessy/ddns-go \
        --set ingress.enabled=true \
        --set ingress.hostname=ddns-go.example.com
      ```
3. change password
    * ```shell
      # create a secret
      kubectl -n basic-components create secret generic ddns-go-credentials \
        --from-literal=username=admin \
        --from-literal=password=$(tr -dc A-Za-z0-9 < /dev/urandom | head -c 16)
      # read password from secret
      PASSWORD=$(kubectl -n basic-components get secret ddns-go-credentials -o jsonpath='{.data.password}' | base64 -d)
      # reset password
      kubectl -n basic-components exec -it deployment/ddns-go -- /app/ddns-go -resetPassword $PASSWORD
      # delete old pod to apply new password
      kubectl -n basic-components delete pod -l app.kubernetes.io/name=ddns-go
      ```