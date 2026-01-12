GOLANG_VERSION := 1.25.5
ALPINE_VERSION := 3.23

APP_NAME := owrap
APP_VERSION := 0.4.1
# + vars.go + README.md

GIT_REPO := github.com/michalswi/owrap
DOCKER_REPO := michalsw

PORT := 8080

.DEFAULT_GOAL := help
.PHONY: build-mac build-linux go-build docker-build

help:
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n\nTargets:\n"} /^[a-zA-Z_-]+:.*?##/ \
	{ printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

build-mac: ## Build for mac
	CGO_ENABLED=0 go build -a \
	-ldflags "-s -w -X 'main.Version=$(APP_VERSION)'" \
	-o $(APP_NAME)_macos_arm64
	sha256sum $(APP_NAME)_macos_arm64 > $(APP_NAME)_macos_arm64.sha256

build-linux: ## Build for linux
	GOOS=linux GOARCH=amd64 go build -a \
	-ldflags "-s -w -X 'main.Version=$(APP_VERSION)'" \
	-o $(APP_NAME)_linux_amd64
	sha256sum $(APP_NAME)_linux_amd64 > $(APP_NAME)_linux_amd64.sha256

go-build: ## Build binary
	CGO_ENABLED=0 \
	go build \
	-v \
	-ldflags "-s -w -X 'main.Version=$(APP_VERSION)'" \
	-o $(APP_NAME)-${APP_VERSION}

docker-build: ## Build linux and arm docker images
	docker buildx build \
	--platform linux/amd64,linux/arm64 \
	--pull \
	--build-arg GOLANG_VERSION="$(GOLANG_VERSION)" \
	--build-arg ALPINE_VERSION="$(ALPINE_VERSION)" \
	--build-arg APP_NAME="$(APP_NAME)" \
	--build-arg APP_VERSION="$(APP_VERSION)" \
	--label="build.version=$(APP_VERSION)" \
	--tag "$(DOCKER_REPO)/$(APP_NAME):latest" \
	--push \
	.
