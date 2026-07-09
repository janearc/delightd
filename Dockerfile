# Stage 1: Build the static binary
# Pinned to a Go patch >= 1.26.5: 1.26.4 and earlier carry GO-2026-5856 (an Encrypted
# Client Hello privacy leak in crypto/tls), which govulncheck finds REACHABLE from the
# daemon's TLS paths. Pinning the patch here fixes it for the shipped binary; the local
# dev toolchain bump is tracked separately (sprints). Bump deliberately, like the other
# pins in this stage.
#
# Pinned by digest, not just tag: a tag is mutable (golang:1.26.5-alpine can be
# repointed upstream to a different image), a digest is not -- this build reproduces the
# same base bytes every time until the digest is deliberately bumped alongside the tag.
# Resolved via `docker pull golang:1.26.5-alpine && docker inspect --format='{{index
# .RepoDigests 0}}' golang:1.26.5-alpine`; re-resolve the same way on every version bump.
FROM golang:1.26.5-alpine@sha256:0178a641fbb4858c5f1b48e34bdaabe0350a330a1b1149aabd498d0699ff5fb2 AS builder

# Ensure we have git to pull module dependencies if needed
RUN apk add --no-cache git

WORKDIR /src

# Codegen toolchain (cached layer): buf + the Go plugin. Protobuf bindings are
# generated at build from the vendored proto and never committed, so this stage
# is what produces gen/ inside the image. protoc-gen-go is pinned to the SAME
# version as the google.golang.org/protobuf runtime the binary links (v1.36.11):
# the generator and the runtime are the same module, so pinning them together keeps
# generated code and runtime in lockstep -- @latest would let a silent generator bump
# desync the two. Bumping one means bumping the other (and go.mod). test/gen asserts
# buf.gen.go.yaml (this image's Go-only template) stays in sync with buf.gen.yaml.
RUN go install github.com/bufbuild/buf/cmd/buf@v1.71.0 \
 && go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.11
ENV PATH="/go/bin:${PATH}"

# Cache dependencies to optimize build times across the fleet
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Generate the Go protobuf bindings (gen/go is gitignored) before compiling. Use the
# Go-only template: the full buf.gen.yaml also runs protoc-gen-rust-serde for the
# committed gen/rust crate, whose plugin is not installed here and whose output the Go
# binary never imports. The image regenerates only what it compiles.
RUN buf generate --template buf.gen.go.yaml

# Build a purely static binary
# CGO_ENABLED=0 ensures no dynamic linking to C libraries (glibc/musl)
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o delightd ./cmd/delightd

# Bake delightd's view of the kube universe into a commit-stamped tree so the running
# container is pinned to the image, not to the mutable host filesystem: changing what a
# running orchestrator does is a rebuild, never a stray local edit. The stamp is legible
# from inside (readlink /etc/delightd/kube -> kube-<sha>), so drift against the on-disk
# source is checkable. GIT_SHA is passed by the build wrapper (git rev-parse --short HEAD,
# plus a -dirty suffix on an unclean tree). It is REQUIRED and has no default: a build that
# forgets it fails here rather than baking a lying "kube-" stamp -- an unstamped orchestrator
# image is worse than a failed build. The tree -- symlink included -- is assembled here
# because the scratch runtime stage has no shell to run ln/mkdir; a relative symlink survives
# the COPY into scratch.
ARG GIT_SHA
# Shape-checked, not just non-empty: a typo'd or hand-crafted GIT_SHA (spaces, a path, an
# injection attempt into the mkdir/cp lines below) is worth catching here rather than baking
# a directory name that lies about being a commit stamp. The wrapper produces `git rev-parse
# --short HEAD`, optionally `-dirty` on an unclean tree (scripts/delightd's git_sha) -- lower
# hex, 4-40 chars (short SHAs vary in length; 40 covers a full SHA too), with an optional
# `-dirty` suffix.
RUN test -n "$GIT_SHA" || { echo "ERROR: GIT_SHA build-arg is required (--build-arg GIT_SHA=\$(git rev-parse --short HEAD))" >&2; exit 1; } \
 && echo "$GIT_SHA" | grep -Eq '^[0-9a-f]{4,40}(-dirty)?$' || { echo "ERROR: GIT_SHA=\"$GIT_SHA\" is not a short-hex commit stamp (want e.g. a1b2c3d or a1b2c3d-dirty)" >&2; exit 1; } \
 && mkdir -p /out/etc/delightd/kube-${GIT_SHA} \
 && cp meubilair.yaml /out/etc/delightd/kube-${GIT_SHA}/meubilair.yaml \
 && cp -r kube /out/etc/delightd/kube-${GIT_SHA}/kube \
 && ln -s kube-${GIT_SHA} /out/etc/delightd/kube \
 && cp delight.yaml /out/etc/delightd/delight.yaml \
 && cp mcp.json /out/etc/delightd/mcp.json

# Stage 2: The microscopic runtime container
# 'scratch' is a literally empty filesystem. 0 bytes. Maximum security.
FROM scratch

# Copy the statically linked binary from the builder stage
COPY --from=builder /src/delightd /usr/local/bin/delightd

# delightd's baked config: the commit-stamped kube universe (symlink included) plus the
# roster. Read-only by virtue of the image; docker-compose.yml does not mount anything
# over /etc/delightd, so nothing on the host shadows it.
COPY --from=builder /out/etc/delightd /etc/delightd

# Expose the daemon's control port (matches config.DefaultControlPort, compose, and kube).
EXPOSE 8088

# Self-probe: the runtime is scratch (no shell, no curl), so the exec-form check runs the
# delightd binary against itself -- `delightd healthcheck` is GET /readyz on the loopback
# control port, mapped to exit 0/1 (cmd/delightd/healthcheck.go). --start-period gives the
# daemon room to reach the apiserver on cold start before a slow first check counts against it.
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
  CMD ["/usr/local/bin/delightd", "healthcheck"]

# Execute the binary
ENTRYPOINT ["/usr/local/bin/delightd"]
