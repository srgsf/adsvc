# adsvc proxy + the minimal static ffmpeg (scripts/build-ffmpeg.sh) on scratch.
# Built by goreleaser (dockers_v2) or `make docker`; either way the binaries are prebuilt.

# Filesystem skeleton, prepared on the build platform so no target-platform RUN is needed
# (linux/arm/v7 would otherwise need emulation).
FROM --platform=$BUILDPLATFORM alpine:3.24 AS rootfs
RUN apk add --no-cache ca-certificates tzdata \
	&& mkdir -p /rootfs/etc/ssl/certs /rootfs/usr/share /rootfs/data /rootfs/tmp \
	&& cp /etc/ssl/certs/ca-certificates.crt /rootfs/etc/ssl/certs/ \
	&& cp -r /usr/share/zoneinfo /rootfs/usr/share/ \
	&& chmod 1777 /rootfs/tmp

FROM scratch
ARG TARGETPLATFORM
ARG TARGETARCH
ARG TARGETVARIANT
ARG BINARY=adsvc

COPY --from=rootfs /rootfs/ /
COPY ${TARGETPLATFORM}/${BINARY} /usr/local/bin/adsvc
# next to adsvc: the proxy uses it by default
COPY .cache/ffmpeg/bin/linux_${TARGETARCH}${TARGETVARIANT}/ffmpeg /usr/local/bin/ffmpeg

WORKDIR /data
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/adsvc"]
# The default listen address is loopback; inside a container it has to be all interfaces.
CMD ["proxy", "-listen", ":8080", "-data-dir", "/data"]
