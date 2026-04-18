FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /bin/dococr ./cmd/dococr

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /bin/dococr /usr/local/bin/dococr
ENTRYPOINT ["dococr"]
