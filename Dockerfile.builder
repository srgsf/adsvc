# Build environment: goreleaser for the Go binaries, zig as a static cross-compiler and
# LLVM binutils for the ffmpeg builds. Runs on the host platform.
FROM goreleaser/goreleaser:latest
RUN apk add --no-cache bash make perl pkgconf xz file nasm zig llvm
