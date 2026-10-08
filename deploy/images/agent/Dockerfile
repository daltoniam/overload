FROM golang:1.27.1 AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /overload-agent ./cmd/overload-agent

FROM us-central1-docker.pkg.dev/k8s-staging-images/agent-sandbox/sandboxd:latest-main
USER root
RUN apt-get update && apt-get install -y --no-install-recommends git ripgrep tar && rm -rf /var/lib/apt/lists/* && mkdir -p /work && chown 1000:1000 /work
COPY --from=builder /overload-agent /usr/local/bin/overload-agent
USER 1000
