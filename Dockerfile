FROM golang:1.26-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/tollgate ./cmd/tollgate

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/tollgate /usr/local/bin/tollgate
COPY config/tollgate.yaml /etc/tollgate/tollgate.yaml

ENV TOLLGATE_CONFIG=/etc/tollgate/tollgate.yaml
EXPOSE 8090
USER nonroot:nonroot

HEALTHCHECK --interval=10s --timeout=3s --start-period=5s CMD ["/usr/local/bin/tollgate", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/tollgate"]
