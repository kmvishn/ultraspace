VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
HOST    ?= ubuntu@server.example.com
SSH_KEY ?= $(HOME)/.ssh/id_ed25519
SSH     := ssh -o IdentitiesOnly=yes -i $(SSH_KEY)
SCP     := scp -o IdentitiesOnly=yes -i $(SSH_KEY)

.PHONY: build build-arm64 run test deploy clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/ultraspace .

build-arm64:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/ultraspace-arm64 .

# Local development server on http://127.0.0.1:4600 with data in ./dev-data
run:
	go run . -config /dev/null -data ./dev-data -debug

test:
	go vet ./...
	go test -race ./...

# Copy the binary to the server and restart the service.
deploy: build
	$(SCP) bin/ultraspace $(HOST):/tmp/ultraspace
	$(SSH) $(HOST) 'sudo install -m 755 /tmp/ultraspace /usr/local/bin/ultraspace && rm /tmp/ultraspace && sudo systemctl restart ultraspace && systemctl is-active ultraspace'

clean:
	rm -rf bin dev-data
