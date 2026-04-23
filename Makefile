build:
	go build -o tfpilot ./cmd/tfpilot/

run: build
	PATH="$(HOME)/bin:$(PATH)" ./tfpilot

install: build
	cp tfpilot $(HOME)/bin/tfpilot

test:
	go test ./...

vet:
	go vet ./...

.PHONY: build run install test vet
