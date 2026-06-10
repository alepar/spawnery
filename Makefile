# Artifacts only — incremental builds. Actions live in the Justfile.
GO_SRCS := $(shell find . -name '*.go' -not -path './web/*')

.PHONY: build images gen clean

build: bin/spawnlet bin/spawnctl bin/authsvc bin/spawnery-ca   # the host-run binaries

bin/%: $(GO_SRCS) | bin
	go build -o $@ ./cmd/$*

# Proto codegen — stamp keyed on .proto sources + buf config.
gen: .make/gen
.make/gen: $(shell find proto -name '*.proto') buf.gen.yaml buf.yaml | .make
	buf generate && touch $@

# Image stamps — rebuild an image only when its build context changes.
images: .make/img-sidecar .make/img-stubagent .make/img-agent
.make/img-sidecar:   deploy/sidecar/Dockerfile   $(GO_SRCS) | .make ; docker build -t spawnery/sidecar:dev   -f $< . && touch $@
.make/img-stubagent: deploy/stubagent/Dockerfile $(GO_SRCS) | .make ; docker build -t spawnery/stubagent:dev -f $< . && touch $@
# The agent image now ships opencode + tmux (replacing goose). Tag stays generic.
.make/img-agent:     deploy/agent/Dockerfile     $(GO_SRCS) | .make ; docker build -t spawnery/agent:dev     -f $< . && touch $@

bin:    ; @mkdir -p bin
.make:  ; @mkdir -p .make
clean:  ; rm -rf bin .make
