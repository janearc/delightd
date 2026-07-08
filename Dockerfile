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

# Stage 2: The microscopic runtime container
# 'scratch' is a literally empty filesystem. 0 bytes. Maximum security.
FROM scratch

# Copy the statically linked binary from the builder stage
COPY --from=builder /src/delightd /usr/local/bin/delightd

# Expose the daemon's internal control port
EXPOSE 8080

# Execute the binary
ENTRYPOINT ["/usr/local/bin/delightd"]
