FROM golang:1.25-alpine AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.Version=${VERSION}" -o /out/theses ./cmd/theses
# The distroless runtime has no shell, so the data directory is created here with
# the nonroot uid and copied in.
RUN mkdir -p /skel/data && chown -R 65532:65532 /skel

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/theses /theses
COPY --from=build --chown=65532:65532 /skel/data /data
ENV THESES_BIND=:8080 THESES_DATA_DIR=/data
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/theses"]
