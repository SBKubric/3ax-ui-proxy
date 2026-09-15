BINARY   := 3ax-ui
TARGET   := target
TMP      := tmp
MODULE   := github.com/coinman-dev/3ax-ui/v2/config

# Version: prefer git tag, fallback to config/version file
VERSION  := $(shell git describe --tags --always --dirty 2>/dev/null || cat config/version)
LDFLAGS  := -X '$(MODULE).version=$(VERSION)'

.PHONY: all build clean tmp-dir target-dir e2e

all: build

## build — compile binary into target/ with version from git tag
build: target-dir
	go build -ldflags "$(LDFLAGS)" -o $(TARGET)/$(BINARY) .

## run — build and run from target/
run: build
	./$(TARGET)/$(BINARY)

## e2e — Playwright harness against this repo's own Docker image
## (docs/agents/testing.md, "E2E tests"). Builds the image, starts the panel
## from e2e/docker-compose.yml with a fresh database, installs the e2e/
## Node toolchain and Chromium, runs the specs, and always tears the stack
## (and its database volume) down — even when the tests fail — while
## preserving the tests' exit code.
##
## Browser install tries `--with-deps` first (apt-get's Chromium's system
## libraries as root — what CI, e.g. GitHub Actions ubuntu-latest, has) and
## falls back to a plain browser-only install if that fails, which is what
## a host with no passwordless sudo needs when those libraries are already
## present (verified on this Debian host: Chromium launches fine without
## --with-deps here).
e2e:
	set -e; \
	trap 'docker compose -f e2e/docker-compose.yml down -v' EXIT; \
	docker compose -f e2e/docker-compose.yml up --build --wait -d; \
	( cd e2e && npm ci && (npx playwright install --with-deps chromium || npx playwright install chromium) && npx playwright test )

## clean — remove target/ and tmp/
clean:
	rm -rf $(TARGET) $(TMP)

## tmp-dir / target-dir — create dirs if missing
tmp-dir:
	@mkdir -p $(TMP)

target-dir:
	@mkdir -p $(TARGET)
