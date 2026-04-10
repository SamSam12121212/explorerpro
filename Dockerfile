FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /bin/api    ./cmd/api
RUN CGO_ENABLED=0 go build -o /bin/worker ./cmd/worker
RUN CGO_ENABLED=0 go build -o /bin/gitservice ./cmd/git
RUN CGO_ENABLED=0 go build -o /bin/wsserver ./cmd/wsserver
RUN CGO_ENABLED=0 go build -o /bin/docsplitter ./cmd/docsplitter
RUN CGO_ENABLED=0 go build -o /bin/documenthandler ./cmd/documenthandler

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata git poppler-utils
COPY --from=build /bin/api    /usr/local/bin/api
COPY --from=build /bin/worker /usr/local/bin/worker
COPY --from=build /bin/gitservice /usr/local/bin/gitservice
COPY --from=build /bin/wsserver /usr/local/bin/wsserver
COPY --from=build /bin/docsplitter /usr/local/bin/docsplitter
COPY --from=build /bin/documenthandler /usr/local/bin/documenthandler
ENTRYPOINT ["api"]
