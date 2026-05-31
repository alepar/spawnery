# Artifacts only — incremental builds. Actions live in the Justfile.
GO_SRCS := $(shell find . -name '*.go' -not -path './web/*')

.PHONY: build images gen clean

build: bin/spawnlet bin/spawnctl          # the host-run binaries

bin/%: $(GO_SRCS) | bin
	go build -o $@ ./cmd/$*

# Proto codegen — stamp keyed on .proto sources + buf config.
gen: .make/gen
.make/gen: $(shell find proto -name '*.proto') buf.gen.yaml buf.yaml | .make
	buf generate && touch $@

# Image stamps — rebuild an image only when its build context changes.
images: .make/img-sidecar .make/img-stubagent .make/img-goose
.make/img-sidecar:   deploy/sidecar/Dockerfile   $(GO_SRCS) | .make ; docker build -t spawnery/sidecar:dev   -f $< . && touch $@
.make/img-stubagent: deploy/stubagent/Dockerfile $(GO_SRCS) | .make ; docker build -t spawnery/stubagent:dev -f $< . && touch $@
.make/img-goose:     deploy/agent/Dockerfile               | .make ; docker build -t spawnery/goose:dev     -f $< . && touch $@

bin:    ; @mkdir -p bin
.make:  ; @mkdir -p .make
clean:  ; rm -rf bin .make
