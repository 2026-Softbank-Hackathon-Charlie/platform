# The deploy E2E's in-cluster helpers: gitserver and proxy, built for Linux
# by run.sh into the build context.
FROM alpine:3.24.3@sha256:be2656061b536c1883c4034790e21d0a09c5aaaf51968b89368a3c0371ea2c9a
RUN apk add --no-cache git git-daemon # git-daemon has git-http-backend
COPY gitserver proxy /usr/local/bin/
USER 65532:65532
