# Stage 1: Build the Go application
# Pin the builder to the build host's native platform and cross-compile to the
# requested target. This avoids running the (slow) Go toolchain under QEMU when
# building multi-arch images with buildx.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder

# Provided automatically by buildx for each target platform.
ARG TARGETOS
ARG TARGETARCH

# Set the Current Working Directory inside the container
WORKDIR /app

# Copy go mod and sum files
COPY go.mod go.sum ./

# Download all dependencies. Dependencies will be cached if the go.mod and go.sum files are not changed
RUN go mod download

# Copy the source from the current directory to the Working Directory inside the container
COPY . .

# Build the Go app for the requested target platform. goshs is cgo-free, so
# CGO_ENABLED=0 yields a static binary that runs in the minimal alpine stage.
# -cover is a no-op unless GOCOVERDIR is set at runtime, so it is safe to keep
# on for production images too.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} go build -cover -o /goshs .

# Stage 2: Create a minimal runtime image
FROM alpine:latest

# Set the Current Working Directory inside the container
WORKDIR /root/

# Copy the Pre-built binary file from the previous stage
COPY --from=builder /goshs .

# Coverage drop dir: integration tests bind-mount a host path here and
# read the emitted covdata after the container shuts down gracefully.
# The dir is world-writable so the non-root user (1000:1000) the tests
# run as can write to it.
ENV GOCOVERDIR=/covdata
RUN mkdir -p /covdata && chmod 0777 /covdata

# Command to run the executable
ENTRYPOINT ["./goshs"]
