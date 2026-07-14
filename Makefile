# Artifacts only — incremental builds. Actions live in the Justfile.
GO_SRCS := $(shell find . -name '*.go' -not -path './web/*')

.PHONY: build images gen clean

build: bin/spawnlet bin/spawnctl bin/authsvc bin/spawnery-ca   # the host-run binaries

bin/%: $(GO_SRCS) | bin
	go build -o $@ ./cmd/$*

# Proto codegen is intentionally unstamped: tracked outputs must be regenerated on every invocation
# so CI and reviewers can detect toolchain drift rather than trusting a local timestamp.
gen: | .make
	buf generate
	@test -x sdk/ts/node_modules/.bin/protoc-gen-es || \
		(echo "[gen] missing TS generator (run: npm ci --workspace @spawnery/client)" >&2; exit 1)
	@echo "[gen] TS (protoc-gen-es) -> sdk/ts/src/gen"
	buf generate --template sdk/ts/buf.gen.yaml

DOCKER ?= docker

# Image stamps — rebuild an image only when its build context changes. Each image's deploy/
# assets (launcher, entrypoints, configs) are COPY'd into it, so they are stamp deps too —
# without them a launch-script edit never triggered a rebuild.
#
# Self-healing: a stamp records "image built", but the Docker image can vanish out-of-band —
# e.g. enabling userns-remap shifts the daemon's storage root and orphans every prior build,
# yet the stamps still look fresh. On every make run, drop any stamp whose image is no longer
# present in the daemon so its target rebuilds. Only probes images that have a stamp (cheap;
# skipped on a clean tree), and a stopped daemon counts as "missing" → safe rebuild.
$(foreach i,sidecar stubagent agent,$(if $(wildcard .make/img-$(i)),$(if $(shell $(DOCKER) image inspect spawnery/$(i):dev >/dev/null 2>&1 && echo ok),,$(shell rm -f .make/img-$(i)))))

DEPLOY_AGENT_SRCS := $(shell find deploy/agent -type f)
images: .make/img-sidecar .make/img-stubagent .make/img-agent
.make/img-sidecar:   deploy/sidecar/Dockerfile   $(GO_SRCS) | .make ; $(DOCKER) build -t spawnery/sidecar:dev   -f $< . && touch $@
.make/img-stubagent: deploy/stubagent/Dockerfile $(shell find deploy/stubagent -type f) $(DEPLOY_AGENT_SRCS) $(GO_SRCS) | .make ; $(DOCKER) build -t spawnery/stubagent:dev -f $< . && touch $@
# The agent image now ships opencode + tmux (replacing goose). Tag stays generic.
.make/img-agent:     deploy/agent/Dockerfile     $(DEPLOY_AGENT_SRCS) $(GO_SRCS) | .make ; $(DOCKER) build -t spawnery/agent:dev     -f $< . && touch $@

bin:    ; @mkdir -p bin
.make:  ; @mkdir -p .make
clean:  ; rm -rf bin .make
