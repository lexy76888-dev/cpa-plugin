.DEFAULT_GOAL := build
PLUGIN_NAME = cpa-quota
VERSION ?= dev
BUILD_DIR ?= dist/linux-amd64
GOOS = linux
GOARCH = amd64
GO_LDFLAGS ?= -s -w -X main.pluginVersion=$(VERSION)
PLUGIN_OUTPUT ?= $(BUILD_DIR)/$(PLUGIN_NAME).so
PLUGIN_HEADER = $(basename $(PLUGIN_OUTPUT)).h
ARCHIVE_NAME ?= $(PLUGIN_NAME)_$(VERSION)_$(GOOS)_$(GOARCH).zip
ARCHIVE_PATH ?= $(BUILD_DIR)/$(ARCHIVE_NAME)
CHECKSUM_PATH ?= $(ARCHIVE_PATH).sha256
CHECKSUMS_PATH ?= $(BUILD_DIR)/checksums.txt

.PHONY: target build check test vet clean package checksums

target:
	@test "$(GOOS)/$(GOARCH)" = "linux/amd64" || (echo "Only linux/amd64 is supported" >&2; exit 1)

build: target
	mkdir -p $(dir $(PLUGIN_OUTPUT))
	CGO_ENABLED=1 GOOS=$(GOOS) GOARCH=$(GOARCH) go build -trimpath -buildmode=c-shared -ldflags "$(GO_LDFLAGS)" -o $(PLUGIN_OUTPUT) .
	rm -f $(PLUGIN_HEADER)

check: vet build
	@test -z "$$(gofmt -l .)"

test:
	go test ./...

vet: target
	CGO_ENABLED=1 GOOS=$(GOOS) GOARCH=$(GOARCH) go vet ./...

package: build
	go run ./.github/scripts/package-release.go -library "$(PLUGIN_OUTPUT)" -archive "$(ARCHIVE_PATH)" -checksum "$(CHECKSUM_PATH)"

checksums: package
	cat $(BUILD_DIR)/*.zip.sha256 | sort -k 2 > "$(CHECKSUMS_PATH)"

clean:
	rm -rf $(BUILD_DIR)
