PATCH           := $(shell  date -u "+%Y%m%d-%H%M%S")
SHORT_COMMIT    := $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)
LEARN_THEME_TAG := 2.2.0

LD_FLAGS_PKG ?= main
LD_FLAGS :=
LD_FLAGS +=  -X "$(LD_FLAGS_PKG).PatchVersion=$(PATCH)"
LD_FLAGS +=  -X "$(LD_FLAGS_PKG).PatchVersionSuffix=$(SHORT_COMMIT)"

.PHONY: all
all: get build lint test

.PHONE: get
get:
	dep ensure

.PHONY: build
build:
	go install -ldflags '$(LD_FLAGS)' ./...

.PHONY: test
test:
	go test -v ./...

.PHONY: lint
lint:
	go list ./... | grep -v vendor | xargs go vet
	go list ./... | grep -v vendor | xargs golint

.PHONY: install
install:
	go get github.com/golang/dep/cmd/dep
	go get golang.org/x/lint/golint
	@echo for building docs, manually install hugo for your OS from: https://github.com/gohugoio/hugo/releases

.PHONY: site
site:
	cd site && rm -rf themes/
	mkdir -p site/themes
	git clone https://github.com/matcornic/hugo-theme-learn site/themes/learn
	(cd site/themes/learn && git checkout -q $(LEARN_THEME_TAG) && rm -rf exampleSite  && rm -f images/* && rm -f CHANGELOG.md netlify.toml wercker.yaml .grenrc.yml)
	cd site && hugo

.PHONY: clean
clean:
	rm -rf vendor/
	rm -rf site/themes
	rm -rf site/public