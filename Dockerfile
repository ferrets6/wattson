# Multi-stage: static Go build (no CGO, modernc.org/sqlite is pure Go),
# minimal distroless final image. The frontend (cmd/wattson/web/) is
# embedded in the binary via go:embed, no need to copy it into the final stage.

FROM golang:1.27.1-bookworm AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/wattson ./cmd/wattson

FROM gcr.io/distroless/static-debian12:nonroot
# Writable by the nonroot user: the default DB_PATH ("./wattson.db") lands
# here unless overridden by a volume mounted elsewhere in the compose file.
WORKDIR /home/nonroot
COPY --from=build /out/wattson /wattson

EXPOSE 8080
ENTRYPOINT ["/wattson"]
