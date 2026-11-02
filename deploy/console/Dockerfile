# The HopCharlie console (frontend repo) as static files. The build context is
# the frontend's dist/ directory; deploy/aws/push-console.sh builds it with
# VITE_PLATFORM_URL set to the platform API. The console uses hash routes, so a
# plain file server is enough.
FROM caddy:2.11.7-alpine
COPY . /srv
EXPOSE 80
CMD ["caddy", "file-server", "--root", "/srv", "--listen", ":80"]
