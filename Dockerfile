ARG GOLANG_VERSION
ARG ALPINE_VERSION

## build
FROM golang:${GOLANG_VERSION}-alpine${ALPINE_VERSION} AS builder

ARG APP_VERSION
ARG APP_NAME

RUN apk --no-cache add make git; \
    adduser -D -h /dummy dummy

USER dummy
WORKDIR /dummy

COPY --chown=dummy Makefile Makefile
COPY --chown=dummy main.go main.go
COPY --chown=dummy banner.go banner.go
COPY --chown=dummy comm.go comm.go
COPY --chown=dummy fileextract.go fileextract.go
COPY --chown=dummy help.go help.go
COPY --chown=dummy vars.go vars.go
COPY --chown=dummy agent_worker.go agent_worker.go
COPY --chown=dummy agent_api.go agent_api.go

COPY --chown=dummy go.mod go.mod
COPY --chown=dummy go.sum go.sum

COPY --chown=dummy utils utils
COPY --chown=dummy webstatic webstatic
COPY --chown=dummy prompts prompts

RUN go mod download

RUN make go-build

## execute
FROM alpine:${ALPINE_VERSION}

ARG APP_VERSION
ARG APP_NAME

ENV OLLAMA_URL ""
ENV OLLAMA_MODEL ""
ENV WEB_BIND ""

# root to install packages
USER root
RUN apk --no-cache add \
bind-tools \
curl \
iputils-ping \
nmap \
netcat-openbsd \
jq \
busybox-extras \
openssl

RUN adduser -D -h /app app
USER app
WORKDIR /app

COPY --from=builder /dummy/prompts ./prompts
COPY --from=builder /dummy/${APP_NAME}-${APP_VERSION} ./${APP_NAME}
CMD ["./owrap", "-web"]
