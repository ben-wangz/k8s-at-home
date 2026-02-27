ARG PODMAN_IMAGE=m.daocloud.io/quay.io/podman/stable:v5.6.1
FROM ${PODMAN_IMAGE}

LABEL maintainer="k8s-at-home"

# Disable netns="host" to use default bridge mode
RUN sed -i 's/^netns="host"$/# netns="host"  # Commented out to use default bridge mode for proper port mapping/' /etc/containers/containers.conf

# Setup environment variables
ENV PATH="$PATH:/opt/nodejs/bin:/usr/local/go/bin" \
    LANG=en_US.UTF-8 \
    LC_ALL=en_US.UTF-8

# Install system packages, openssh-server, python, nodejs, go and locale support
ARG NODEJS_VERSION=v24.13.1
ARG GO_VERSION=1.23.5
RUN dnf update -y \
    && dnf install -y procps ncurses curl wget which screen tree unzip openssl jq yq \
       glibc-locale-source glibc-langpack-zh \
       openssh-server vim \
       bind-utils telnet iputils \
       poppler-utils \
       python3 python3-pip \
    && localedef -c -f UTF-8 -i en_US en_US.UTF-8 \
    && curl -fsSL https://nodejs.org/dist/${NODEJS_VERSION}/node-${NODEJS_VERSION}-linux-x64.tar.xz -o /tmp/nodejs.tar.xz \
    && mkdir -p /opt/nodejs \
    && tar -xJf /tmp/nodejs.tar.xz -C /opt/nodejs --strip-components=1 \
    && rm /tmp/nodejs.tar.xz \
    && curl -fsSL https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz -o /tmp/go.tar.gz \
    && tar -C /usr/local -xzf /tmp/go.tar.gz \
    && rm /tmp/go.tar.gz \
    && pip3 install --no-cache-dir paper-search-mcp \
    && dnf clean all

# Configure SSH and setup login message
RUN mkdir -p /var/run/sshd \
    && sed -i 's/#PermitRootLogin yes/PermitRootLogin without-password/' /etc/ssh/sshd_config \
    && sed -i 's/#PasswordAuthentication yes/PasswordAuthentication no/' /etc/ssh/sshd_config \
    && sed -ri 's/session required pam_loginuid.so/#session required pam_loginuid.so/g' /etc/pam.d/sshd \
    && echo "export PATH=\"${PATH}\"" > /etc/profile.d/custom-path.sh \
    && chmod +x /etc/profile.d/custom-path.sh

# Copy documentation and motd
RUN mkdir -p /opt/codespace/docs
COPY --chmod=0644 buitin-docs/ /opt/codespace/docs/
COPY --chmod=0644 motd /etc/motd

# Copy bin scripts
COPY --chmod=0755 bin/ /usr/local/bin/

# Copy entrypoint script
COPY --chmod=0755 entrypoint.sh /entrypoint.sh

# Install openvscode-server
ARG OPENVSCODE_VERSION=v1.103.1
RUN curl -LO https://github.com/gitpod-io/openvscode-server/releases/download/openvscode-server-${OPENVSCODE_VERSION}/openvscode-server-${OPENVSCODE_VERSION}-linux-x64.tar.gz \
    && mkdir -p /opt/openvscode-server \
    && tar zxvf openvscode-server-${OPENVSCODE_VERSION}-linux-x64.tar.gz -C /opt/openvscode-server \
    && ln -s /opt/openvscode-server/openvscode-server-${OPENVSCODE_VERSION}-linux-x64 /opt/openvscode-server/current \
    && rm openvscode-server-${OPENVSCODE_VERSION}-linux-x64.tar.gz

# Download tini (moved down to improve cache utilization)
ARG TINI_VERSION=v0.19.0
ADD --chmod=0755 https://github.com/krallin/tini/releases/download/${TINI_VERSION}/tini /tini

ENTRYPOINT ["/tini", "--"]
CMD ["/entrypoint.sh"]
