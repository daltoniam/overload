FROM --platform=$BUILDPLATFORM golang:1.27.1 AS builder
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o /overload ./cmd/overload

FROM gcr.io/distroless/static-debian12:nonroot
ARG VERSION=dev
LABEL org.opencontainers.image.source="https://github.com/daltoniam/overload" \
      org.opencontainers.image.version="$VERSION" \
      org.opencontainers.image.licenses="Apache-2.0"
COPY --from=builder /overload /usr/local/bin/overload
USER 65532:65532
EXPOSE 8082
ENTRYPOINT ["/usr/local/bin/overload"]
CMD ["serve"]
