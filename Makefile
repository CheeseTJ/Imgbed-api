.PHONY: build test run tidy clean

build:
	go build -o bin/imgbed ./cmd/imgbed

test:
	go test ./...

run:
	go run ./cmd/imgbed -config ./config.yaml

tidy:
	go mod tidy

clean:
	rm -rf bin
