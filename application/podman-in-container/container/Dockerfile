ARG PODMAN_IMAGE=m.daocloud.io/quay.io/podman/stable:v5.6.1
FROM ${PODMAN_IMAGE}

LABEL maintainer="k8s-at-home"

# Install system packages, Node.js, pnpm, and Claude Code in fewer layers
RUN dnf update -y \
    && dnf install -y openssh-server procps ncurses curl \
    && dnf clean all \
    && curl -fsSL https://rpm.nodesource.com/setup_lts.x | bash - \
    && dnf install -y nodejs \
    && dnf clean all \
    && npm install -g pnpm

# Setup pnpm environment and install Claude Code (avoid /root as it will be mounted)
ENV PNPM_HOME="/opt/pnpm" \
    PATH="/opt/pnpm:$PATH"
RUN mkdir -p ${PNPM_HOME} \
    && pnpm config set global-bin-dir ${PNPM_HOME} \
    && pnpm add -g @anthropic-ai/claude-code

# Configure SSH
RUN mkdir -p /var/run/sshd \
    && sed -i 's/#PermitRootLogin yes/PermitRootLogin without-password/' /etc/ssh/sshd_config \
    && sed -i 's/#PasswordAuthentication yes/PasswordAuthentication no/' /etc/ssh/sshd_config \
    && sed -ri 's/session required pam_loginuid.so/#session required pam_loginuid.so/g' /etc/pam.d/sshd

# Copy entrypoint script
COPY --chmod=0755 entrypoint.sh /entrypoint.sh

# Download tini (moved down to improve cache utilization)
ARG TINI_VERSION=v0.19.0
ADD --chmod=0755 https://github.com/krallin/tini/releases/download/${TINI_VERSION}/tini /tini

ENTRYPOINT ["/tini", "--"]
CMD ["/entrypoint.sh"]
