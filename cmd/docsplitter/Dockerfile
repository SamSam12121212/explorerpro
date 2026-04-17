FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /bin/docsplitter ./cmd/docsplitter

FROM alpine:3.22
# poppler-utils: docsplitter shells out to `pdftocairo` (internal/docsplitter/service.go)
RUN apk add --no-cache ca-certificates tzdata poppler-utils
COPY --from=build /bin/docsplitter /usr/local/bin/docsplitter
ENTRYPOINT ["docsplitter"]
