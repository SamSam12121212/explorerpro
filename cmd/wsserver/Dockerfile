FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /bin/wsserver ./cmd/wsserver

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /bin/wsserver /usr/local/bin/wsserver
ENTRYPOINT ["wsserver"]
