.PHONY: all build build-wackypub build-tools test vet fmt tidy clean check proto

BIN_DIR := ./bin
SUBMODULES := tools/files-rw tools/wackyproc tools/wackydiscord

all: build

build: build-wackypub build-tools

build-wackypub:
	@mkdir -p $(BIN_DIR)
	go build -o $(BIN_DIR)/wackypub .

build-tools:
	@mkdir -p $(BIN_DIR)
	go build -C tools/files-rw -o ../../bin/files-rw .
	go build -C tools/wackyproc -o ../../bin/wackyproc .
	go build -C tools/wackydiscord -o ../../bin/wackydiscord .

test:
	go test ./...
	@for dir in $(SUBMODULES); do \
		echo "=== Testing $$dir ==="; \
		go test -C $$dir ./... || exit 1; \
	done

vet:
	go vet ./...
	@for dir in $(SUBMODULES); do \
		echo "=== Vetting $$dir ==="; \
		go vet -C $$dir ./... || exit 1; \
	done

fmt:
	go fmt ./...
	@for dir in $(SUBMODULES); do \
		echo "=== Formatting $$dir ==="; \
		go fmt -C $$dir ./... || exit 1; \
	done

tidy:
	go mod tidy
	@for dir in $(SUBMODULES); do \
		echo "=== Tidying $$dir ==="; \
		go -C $$dir mod tidy || exit 1; \
	done

check: fmt vet test
 
proto:
	buf generate

clean:
	rm -rf $(BIN_DIR)
