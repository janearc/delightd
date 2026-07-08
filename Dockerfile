# Stage 1: Build the static binary
FROM golang:1.26-alpine AS builder

# Ensure we have git to pull module dependencies if needed
RUN apk add --no-cache git

WORKDIR /src

# Codegen toolchain (cached layer): buf + the Go plugin. Protobuf bindings are
# generated at build from the vendored proto and never committed, so this stage
# is what produces gen/ inside the image.
RUN go install github.com/bufbuild/buf/cmd/buf@v1.71.0 \
 && go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
ENV PATH="/go/bin:${PATH}"

# Cache dependencies to optimize build times across the fleet
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Generate the protobuf bindings (gen/ is gitignored) before compiling.
RUN buf generate

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
RUN test -n "$GIT_SHA" || { echo "ERROR: GIT_SHA build-arg is required (--build-arg GIT_SHA=\$(git rev-parse --short HEAD))" >&2; exit 1; } \
 && mkdir -p /out/etc/delightd/kube-${GIT_SHA} \
 && cp meubilair.yaml /out/etc/delightd/kube-${GIT_SHA}/meubilair.yaml \
 && cp -r kube /out/etc/delightd/kube-${GIT_SHA}/kube \
 && ln -s kube-${GIT_SHA} /out/etc/delightd/kube \
 && cp delight.yaml /out/etc/delightd/delight.yaml

# Stage 2: The microscopic runtime container
# 'scratch' is a literally empty filesystem. 0 bytes. Maximum security.
FROM scratch

# Copy the statically linked binary from the builder stage
COPY --from=builder /src/delightd /usr/local/bin/delightd

# delightd's baked config: the commit-stamped kube universe (symlink included) plus the
# roster. Read-only by virtue of the image; the host filesystem does not shadow it once
# the runtime stops mounting ~/etc over /etc/delightd (compose change, follow-up diff).
COPY --from=builder /out/etc/delightd /etc/delightd

# Expose the daemon's control port (matches config.DefaultControlPort, compose, and kube).
EXPOSE 8088

# Execute the binary
ENTRYPOINT ["/usr/local/bin/delightd"]
