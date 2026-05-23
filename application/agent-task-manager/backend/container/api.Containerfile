ARG GO_IMAGE=docker.io/library/golang:1.25-bookworm
ARG RUNTIME_IMAGE=gcr.io/distroless/base-debian12

FROM ${GO_IMAGE} AS builder

WORKDIR /src
COPY backend/src/go.mod ./go.mod
COPY backend/src/go.sum ./go.sum
COPY backend/src/cmd ./cmd
COPY backend/src/internal ./internal
COPY backend/src/pkg ./pkg
RUN if [ -f go.sum ]; then go mod download; fi && CGO_ENABLED=0 GOOS=linux GOARCH=$(go env GOARCH) go build -o /out/agent-task-manager-api ./cmd/agent-task-manager-api

FROM ${RUNTIME_IMAGE}

COPY --from=builder /out/agent-task-manager-api /agent-task-manager-api

ENTRYPOINT ["/agent-task-manager-api"]
