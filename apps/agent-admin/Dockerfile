FROM golang:1.25.13-alpine AS build
WORKDIR /src
ENV GOWORK=off
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -mod=readonly -trimpath -ldflags="-s -w" -o /out/agent .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/agent /agent
USER 65532:65532
ENTRYPOINT ["/agent"]
