.PHONY: build test vet run tidy clean

BIN := bin/hostel

build:
	go build -o $(BIN) ./cmd/hostel

test:
	go test ./...

vet:
	go vet ./...

tidy:
	go mod tidy

run: build
	$(BIN) --isolation direct --workspace-root ./.workspace --addr :44772

clean:
	rm -rf bin .workspace
