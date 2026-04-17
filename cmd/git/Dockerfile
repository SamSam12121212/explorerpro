FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /bin/gitservice ./cmd/git

FROM alpine:3.22
# git: gitservice shells out to `git clone` / `git rev-parse` (internal/gitservice/service.go)
RUN apk add --no-cache ca-certificates tzdata git
COPY --from=build /bin/gitservice /usr/local/bin/gitservice
ENTRYPOINT ["gitservice"]
