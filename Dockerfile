FROM golang:1.25.13-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY api/ api/
COPY cmd/ cmd/
COPY internal/ internal/
ARG APP
RUN --mount=type=cache,target=/root/.cache/go-build CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /agent ./cmd/$APP

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /agent /agent
USER 65532:65532
ENTRYPOINT ["/agent"]
