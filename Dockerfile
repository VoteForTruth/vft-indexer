FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /vft-indexer ./cmd/vft-indexer

FROM gcr.io/distroless/static-debian12
COPY --from=build /vft-indexer /vft-indexer
EXPOSE 8080
ENTRYPOINT ["/vft-indexer"]
