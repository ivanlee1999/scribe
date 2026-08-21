# Two stages: build with the toolchain, ship without it. The result is a single static binary on
# a distroless base — a few megabytes, nothing to patch, and nothing to exec into.
FROM golang:1.26-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
# CGO off so the binary does not want a libc that distroless does not have.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /scribe ./cmd/scribe
# Staged here only so the next stage can copy it in already owned by nonroot. Docker seeds a
# named volume from the image's directory, ownership included — but when the image has no such
# directory it creates one owned by root, and the nonroot user cannot write its checkpoint.
RUN mkdir -p /state

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /scribe /scribe
# 65532 is nonroot in distroless. Numeric because COPY --chown resolves names against the target
# image, and relying on that is a silent root-owned directory when it does not.
COPY --from=build --chown=65532:65532 /state /var/lib/scribe
# The state file lives here; mount a volume over it or the checkpoint is lost on every restart,
# which costs a library rescan rather than any data.
VOLUME ["/var/lib/scribe"]
USER nonroot:nonroot
ENTRYPOINT ["/scribe"]
