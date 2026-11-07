VERSION ?= dev
PREFIX  ?= $(HOME)/.local
build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o bin/lls ./cmd/lls
install: build
	install -Dm755 bin/lls $(PREFIX)/bin/lls
indexes: build
	LLS=bin/lls scripts/build-all.sh
cross:
	scripts/cross-compile.sh
.PHONY: build install indexes cross
