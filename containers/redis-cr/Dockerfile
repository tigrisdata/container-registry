# check=skip=SecretsUsedInArgOrEnv

FROM redis:bookworm 

RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        gettext-base \
        && \
    rm -rf /var/lib/apt/lists/*

COPY entrypoint.sh /usr/local/bin/custom-entrypoint.sh
RUN chmod +x /usr/local/bin/custom-entrypoint.sh

EXPOSE 6379

ENTRYPOINT ["/usr/local/bin/custom-entrypoint.sh"]
