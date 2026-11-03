FROM golang:1.26.10-alpine3.24 AS build
WORKDIR /src
COPY go.mod main.go version.txt ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/demo .

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/demo /demo
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/demo"]
