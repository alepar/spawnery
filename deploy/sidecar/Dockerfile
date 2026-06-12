FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -o /sidecar ./cmd/sidecar
FROM gcr.io/distroless/static
COPY --from=build /sidecar /sidecar
ENTRYPOINT ["/sidecar"]
