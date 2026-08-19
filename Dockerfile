# Two stages: build with the toolchain, ship without it. The result is a single static binary on
# a distroless base — a few megabytes, nothing to patch, and nothing to exec into.
FROM golang:1.26-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
# CGO off so the binary does not want a libc that distroless does not have.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /scribe ./cmd/scribe

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /scribe /scribe
# The state file lives here; mount a volume over it or the checkpoint is lost on every restart,
# which costs a library rescan rather than any data.
VOLUME ["/var/lib/scribe"]
USER nonroot:nonroot
ENTRYPOINT ["/scribe"]
