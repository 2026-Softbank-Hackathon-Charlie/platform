# One image with both binaries; compose picks the command.
#
# The binaries are built on the host (deploy/aws/push-image.sh) instead of in a
# build stage: the migration-engine module is private, and a build stage would
# need GitHub credentials inside the image build.
FROM alpine:3.24.3

# rclone: the engine syncs object storage with it when a plan has "files".
RUN apk add --no-cache ca-certificates rclone tzdata \
 && adduser -D -u 10001 -h /home/app app \
 && mkdir -p /data /home/app/.kube /etc/hopcharlie \
 && chown -R app:app /data /home/app

COPY bin/hopcharlie-api bin/hopcharlie-gateway /usr/local/bin/

USER app
WORKDIR /data
ENV HOME=/home/app
