FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal

ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/multienv ./cmd/multienv

FROM scratch
COPY --from=builder /out/multienv /multienv

# State lives on a volume so it survives container recreates.
WORKDIR /data
VOLUME ["/data"]
ENV MULTIENV_STATE_FILE=/data/state.json

# The daemon talks to the host's Docker daemon via the mounted socket.
# Mount with: -v /var/run/docker.sock:/var/run/docker.sock
ENTRYPOINT ["/multienv"]
CMD ["daemon"]
