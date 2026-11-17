# syntax=docker/dockerfile:1
FROM golang:1.27.4 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY migrations ./migrations
RUN CGO_ENABLED=0 go build -trimpath -o /out/reader ./cmd/reader

FROM build AS test
COPY architecture ./architecture
CMD ["go", "test", "-count=1", "./..."]

FROM scratch AS runtime
WORKDIR /app
COPY --link --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --link --from=build /out/reader /app/reader
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/app/reader"]
CMD ["web"]
