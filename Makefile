BINARY   := spotify-connect-streamer-ng
DIST     := dist
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X main.version=$(VERSION)

PLATFORMS := darwin-amd64 darwin-arm64 linux-amd64

.PHONY: all build clean $(PLATFORMS)

all: $(PLATFORMS)

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) .

darwin-amd64:
	GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" \
		-o $(DIST)/$(BINARY)-darwin-amd64 .

darwin-arm64:
	GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" \
		-o $(DIST)/$(BINARY)-darwin-arm64 .

linux-amd64:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" \
		-o $(DIST)/$(BINARY)-linux-amd64 .

clean:
	rm -rf $(DIST) $(BINARY)
