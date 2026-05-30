.PHONY: gen build test test-unit test-e2e images
gen:
	buf generate
build:
	go build ./...
test: test-unit

# Unit suite: fast, hermetic, no Docker and no network.
test-unit:
	go test ./... -count=1

# Build the three container images the e2e suite drives.
images:
	docker build -t spawnery/stubagent:dev -f deploy/stubagent/Dockerfile .
	docker build -t spawnery/sidecar:dev   -f deploy/sidecar/Dockerfile .
	docker build -t spawnery/goose:dev     -f deploy/agent/Dockerfile .

# End-to-end suite: builds the images then runs the //go:build e2e tests
# (real Docker pods + a live OpenRouter round-trip). Requires OPENROUTER_API_KEY.
test-e2e: images
	go test -tags e2e ./... -count=1 -v
