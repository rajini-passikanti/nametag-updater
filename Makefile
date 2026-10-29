VERSION   ?= 1.0.0
KEY_ID    ?= k1
KEY       ?= keys/$(KEY_ID).key
PUBKEYS   ?= keys/$(KEY_ID).pub
RELEASES  ?= releases
CHANNEL   ?= stable
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64
MODULE    := nametag-updater

LDFLAGS = -s -w \
	-X $(MODULE)/internal/version.Version=$(VERSION) \
	-X $(MODULE)/internal/version.PublicKeys=$(shell cat $(PUBKEYS) 2>/dev/null)

.PHONY: all keygen build release serve test test-short vet lint demo clean

all: vet test

keygen: ## Generate a release-signing key pair in keys/
	go run ./cmd/release keygen --key-id $(KEY_ID) --out keys

build: ## Cross-compile the client for every platform into dist/$(VERSION)
	@test -f $(PUBKEYS) || { echo "missing $(PUBKEYS); run 'make keygen' first"; exit 1; }
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; ext=; [ $$os = windows ] && ext=.exe; \
		echo "build $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" \
			-o dist/$(VERSION)/app-$$os-$$arch$$ext ./cmd/app || exit 1; \
	done

release: build ## Build, sign, and publish $(VERSION) to $(RELEASES)
	go run ./cmd/release publish --version $(VERSION) --dist dist/$(VERSION) \
		--key $(KEY) --releases $(RELEASES) --channel $(CHANNEL)

serve: ## Run the update server on 127.0.0.1:8080
	go run ./cmd/update-server --releases $(RELEASES)

test: ## Unit and end-to-end tests
	go test -race ./...

test-short: ## Unit tests only
	go test -short ./...

vet:
	go vet ./...

lint:
	go run honnef.co/go/tools/cmd/staticcheck@v0.8.2 ./...

demo: ## Watch a running program update itself (macOS/Linux)
	./scripts/demo.sh

clean:
	rm -rf dist bin .demo
