GOLANG_VERSION := 1.25.5
ALPINE_VERSION := 3.23

APP_NAME := owrap
APP_VERSION := 0.4.0 # > vars.go + README.md

GIT_REPO := github.com/michalswi/owrap
DOCKER_REPO := michalsw

BUILD_TIME ?= $(shell date -u '+%Y-%m-%d %H:%M:%S')

.DEFAULT_GOAL := help
.PHONY: build_mac build_linux go-build docker-build-arm docker-build-linux docker-run docker-stop

help:
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n\nTargets:\n"} /^[a-zA-Z_-]+:.*?##/ \
	{ printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

build_mac: ## Build for mac
	CGO_ENABLED=0 go build -a \
	-ldflags "-s -w -X 'main.Version=v$(APP_VERSION)'" \
	-X '$(GIT_REPO)/version.BuildTime=$(BUILD_TIME)' \
	-o $(APP_NAME)_macos_arm64
	sha256sum $(APP_NAME)_macos_arm64 > $(APP_NAME)_macos_arm64.sha256
	
build_linux: ## Build for linux
	GOOS=linux GOARCH=amd64 go build -a \
	-ldflags "-s -w -X 'main.Version=v$(APP_VERSION)'" \
	-X '$(GIT_REPO)/version.BuildTime=$(BUILD_TIME)' \
	-o $(APP_NAME)_linux_amd64
	sha256sum $(APP_NAME)_linux_amd64 > $(APP_NAME)_linux_amd64.sha256

go-build:
	CGO_ENABLED=0 \
	go build \
	-v \
	-ldflags "-s -w -X 'main.Version=v$(APP_VERSION)'" \
	-X '$(GIT_REPO)/version.BuildTime=$(BUILD_TIME)' \
	-o $(APP_NAME)-${APP_VERSION}

docker-build-arm: ## Build arm docker image
	docker build \
	--pull \
	--build-arg GOLANG_VERSION="$(GOLANG_VERSION)" \
	--build-arg ALPINE_VERSION="$(ALPINE_VERSION)" \
	--build-arg APP_NAME="$(APP_NAME)" \
	--build-arg APP_VERSION="$(APP_VERSION)" \
	--build-arg BUILD_TIME="$(BUILD_TIME)" \
	--build-arg LAST_COMMIT_TIME="$(LAST_COMMIT_TIME)" \	
	--label="build.version=$(APP_VERSION)" \
	--tag="$(DOCKER_REPO)/$(APP_NAME):latest" \
	.

docker-build-linux: ## Build linux docker image
	docker build \
	--pull \
	--build-arg GOLANG_VERSION="$(GOLANG_VERSION)" \
	--build-arg ALPINE_VERSION="$(ALPINE_VERSION)" \
	--build-arg APP_NAME="$(APP_NAME)" \
	--build-arg APP_VERSION="$(APP_VERSION)" \
	--build-arg BUILD_TIME="$(BUILD_TIME)" \
	--build-arg LAST_COMMIT_TIME="$(LAST_COMMIT_TIME)" \
	--label="build.version=$(APP_VERSION)" \
	--platform linux/amd64 \
	--tag="$(DOCKER_REPO)/$(APP_NAME):latest" \
	.

docker-run: ## Run docker image
	docker run -d --rm \
	--name $(APP_NAME) \
	-p $(PORT):$(PORT) \
	$(DOCKER_REPO)/$(APP_NAME):latest && \
	docker ps

docker-stop: ## Stop running docker
	docker stop $(APP_NAME)
