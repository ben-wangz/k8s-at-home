ARG NGINX_IMAGE=docker.io/library/nginx:stable
FROM ${NGINX_IMAGE}
RUN apt-get update \
    && apt-get install -y moreutils netcat-openbsd curl unzip \
    && apt-get clean

ARG ARIA_NG_DOWNLOAD_URL=https://github.com/mayswind/AriaNg/releases/download/1.3.10/AriaNg-1.3.10.zip
ADD ${ARIA_NG_DOWNLOAD_URL} /tmp/
RUN unzip /tmp/AriaNg-1.3.10.zip -d /usr/share/nginx/html/
COPY nginx.conf /etc/nginx/nginx.conf
COPY --chmod=755 entrypoint.sh /opt/aria-ng/bin/entrypoint.sh

EXPOSE 80

CMD ["/opt/aria-ng/bin/entrypoint.sh"]
