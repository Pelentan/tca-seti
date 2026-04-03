FROM alpine:3.19

RUN apk add --no-cache openssl bash

COPY generate-certs.sh /generate-certs.sh
RUN chmod +x /generate-certs.sh

ENTRYPOINT ["/bin/bash", "/generate-certs.sh"]
