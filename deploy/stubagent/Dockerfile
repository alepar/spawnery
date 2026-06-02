FROM golang:1.25 AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -o /stubagent ./cmd/stubagent
FROM gcr.io/distroless/static
COPY --from=build /stubagent /stubagent
ENTRYPOINT ["/stubagent"]
