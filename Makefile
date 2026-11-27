GO ?= go
PYTHON ?= python3
BIN := bin/quickproxy
ADDR ?= 127.0.0.1:8080

.PHONY: all build login status refresh logout run test clean

all: build

build:
	mkdir -p bin
	$(GO) build -trimpath -ldflags="-s -w" -o $(BIN) ./cmd/quickproxy

login:
	$(PYTHON) auth/quickauth.py upstream-login --provider social

status:
	$(PYTHON) auth/quickauth.py status

refresh:
	$(PYTHON) auth/quickauth.py refresh

logout:
	$(PYTHON) auth/quickauth.py logout

run: build
	./$(BIN) -addr $(ADDR)

test:
	$(GO) test ./...

clean:
	rm -rf bin
