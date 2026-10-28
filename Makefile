.PHONY: install release-artifacts

DIST ?= dist
GO ?= go
GIT ?= git
SBOM_TOOL ?= cyclonedx-gomod

install:
	./install.sh

release-artifacts:
	@VERSION="$(VERSION)" DIST="$(DIST)" BUILD_GO="$(GO)" BUILD_GIT="$(GIT)" \
		SBOM_TOOL="$(SBOM_TOOL)" go run ./scripts/release-artifacts.go
