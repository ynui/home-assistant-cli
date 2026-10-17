# The binary is the repository name; `has` is the short alias installed next to
# it (and the only name the program calls itself in its own output).
BIN := home-assistant-cli
ALIAS := has
DIR := bin
# ~/.local/bin is on PATH here; override for another location.
PREFIX ?= $(HOME)/.local/bin
# Version is injected at build time. Override for a release:
#   make build VERSION=v0.3.0
VERSION ?= dev

.PHONY: all fmt vet lint vuln test build install uninstall run demo clean

all: fmt vet lint test build

fmt:
	gofmt -l -w .

vet:
	go vet ./...

# staticcheck finds what go vet does not: an unused helper, a comparison that
# can never be true, a loop that copies by value what it could point at. Run as
# a module download rather than an install step, so `make` works on a fresh
# clone with nothing but Go.
lint:
	go run honnef.co/go/tools/cmd/staticcheck@v0.8.2 ./...

# govulncheck needs the network and a vulnerability database, so it is not in
# `all`: `make lint` on a plane should still work.
vuln:
	go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...

test:
	go test -race ./...

# Kept out of the repo root so the tree stays clean.
build:
	mkdir -p $(DIR)
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=$(VERSION)" -o $(DIR)/$(BIN) .

# Respects GOBIN/GOPATH, then verifies the binary is actually reachable so a
# stale shell PATH does not leave you thinking it installed.
install:
	@mkdir -p $(PREFIX)
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=$(VERSION)" -o $(PREFIX)/$(BIN) .
	@ln -sf $(PREFIX)/$(BIN) $(PREFIX)/$(ALIAS)
	@echo "installed $(PREFIX)/$(BIN) (alias: $(ALIAS))"
	@case ":$$PATH:" in \
		*":$(PREFIX):"*) echo "$(PREFIX) is on your PATH";; \
		*) echo ""; \
		    echo "$(PREFIX) is NOT on your PATH. Add this line to your ~/.zshrc:"; \
		    echo '    export PATH=$(PREFIX):$$PATH';; \
	esac
	@other=""; \
	IFS=:; for d in $$PATH; do \
		if [ -z "$$other" ] && [ -x "$$d/$(ALIAS)" ] && [ "$$d/$(ALIAS)" != "$(PREFIX)/$(ALIAS)" ]; then \
			other="$$d/$(ALIAS)"; \
		fi; \
	done; \
	if [ -n "$$other" ]; then \
		echo ""; \
		echo "note: '$(ALIAS)' also lives at $$other."; \
		echo "      '$(ALIAS)' runs $$(command -v $(ALIAS)) now."; \
		echo "      'make uninstall' hands the name back."; \
	fi

# Removes exactly what install put there, in the other order, and is happy when
# there is nothing to remove - which is the state after a git clone, and the one
# worth being able to put in a cleanup script.
#
# `make install` is not needed to work on this: `make run ARGS="ui"` and
# `go run .` both use the checkout and leave PATH alone. Install only to get the
# `has` name itself, and uninstall to give it back to brew.
uninstall:
	@removed=0; \
	for f in $(PREFIX)/$(ALIAS) $(PREFIX)/$(BIN); do \
		if [ -e "$$f" ] || [ -L "$$f" ]; then rm -f "$$f" && echo "removed $$f"; removed=1; fi; \
	done; \
	if [ $$removed -eq 0 ]; then echo "nothing installed in $(PREFIX)"; fi

run: build
	./$(DIR)/$(BIN) $(ARGS)

# Records demo/demo.gif. Needs vhs, plus ttyd and ffmpeg under it.
# The tape talks to demo/fakeha - a canned Home Assistant - so no real house and
# no real ~/.config/has are involved.
demo: build
	go build -o $(DIR)/fakeha ./demo/fakeha
	vhs demo/demo.tape

clean:
	go clean
	rm -rf $(DIR)