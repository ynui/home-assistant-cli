# The binary is the repository name; `has` is the short alias installed next to
# it (and the only name the program calls itself in its own output).
BIN := home-assistant-cli
ALIAS := has
DIR := bin
# ~/.local/bin is on PATH here; override for another location.
PREFIX ?= $(HOME)/.local/bin

.PHONY: all fmt vet test build install run demo clean

all: fmt vet test build

fmt:
	gofmt -l -w .

vet:
	go vet ./...

test:
	go test -race ./...

# Kept out of the repo root so the tree stays clean.
build:
	mkdir -p $(DIR)
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(DIR)/$(BIN) .

# Respects GOBIN/GOPATH, then verifies the binary is actually reachable so a
# stale shell PATH does not leave you thinking it installed.
install:
	@mkdir -p $(PREFIX)
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(PREFIX)/$(BIN) .
	@ln -sf $(PREFIX)/$(BIN) $(PREFIX)/$(ALIAS)
	@echo "installed $(PREFIX)/$(BIN) (alias: $(ALIAS))"
	@case ":$$PATH:" in \
		*":$(PREFIX):"*) echo "$(PREFIX) is on your PATH";; \
		*) echo ""; \
		    echo "$(PREFIX) is NOT on your PATH. Add this line to your ~/.zshrc:"; \
		    echo '    export PATH=$(PREFIX):$$PATH';; \
	esac

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