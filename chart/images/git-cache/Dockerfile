ARG BASE_IMAGE=python:3.12-alpine
FROM ${BASE_IMAGE}

RUN apk add --no-cache git git-daemon

COPY git-cache-server.py /usr/local/bin/git-cache-server.py

EXPOSE 8080
ENTRYPOINT ["python3", "/usr/local/bin/git-cache-server.py"]
