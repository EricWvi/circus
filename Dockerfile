FROM node:24-alpine AS web
WORKDIR /src
COPY package.json package-lock.json ./
COPY web/package.json ./web/
COPY gen/ts/package.json ./gen/ts/
RUN npm ci
COPY web ./web
COPY gen/ts ./gen/ts
RUN VITE_CIRCUS_BACKEND=server npm -w @circus/web run build

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY gen/go ./gen/go
COPY web/*.go ./web/
COPY --from=web /src/web/dist ./web/dist
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -tags webui -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/circus ./cmd/circus \
    && mkdir -p /out/data \
    && chown 65532:65532 /out/data

FROM gcr.io/distroless/static-debian12:nonroot
ARG VERSION=dev
LABEL org.opencontainers.image.title="circus" \
      org.opencontainers.image.version="${VERSION}"
COPY --from=build /out/circus /usr/local/bin/circus
COPY --from=build --chown=65532:65532 /out/data /data
ENV XDG_DATA_HOME=/data
VOLUME /data
EXPOSE 8443
ENTRYPOINT ["/usr/local/bin/circus", "server"]
CMD ["--config", "/etc/circus/config.yaml"]
