FROM scratch

LABEL maintainer="estafette.io" \
      description="The estafette-gcloud-quota-exporter turns gcloud quota details into prometheus timeline series"

COPY ca-certificates.crt /etc/ssl/certs/
COPY estafette-gcloud-quota-exporter /

ENTRYPOINT ["/estafette-gcloud-quota-exporter"]
