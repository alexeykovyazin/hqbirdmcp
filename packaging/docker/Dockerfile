# Non-root image. Linux container matrix is best-effort (deferred if Docker is down).
FROM gcr.io/distroless/static:nonroot
WORKDIR /app
COPY fbmcp /app/fbmcp
USER nonroot
VOLUME ["/etc/fbmcp", "/var/lib/fbmcp"]
ENTRYPOINT ["/app/fbmcp"]
CMD ["-config", "/etc/fbmcp/fbmcp.yaml"]
