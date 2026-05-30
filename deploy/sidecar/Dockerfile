FROM golang:1.25 AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -o /sidecar ./cmd/sidecar
FROM gcr.io/distroless/static
COPY --from=build /sidecar /sidecar
ENTRYPOINT ["/sidecar"]
