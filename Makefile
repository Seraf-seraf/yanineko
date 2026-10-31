.PHONY: dev test vet build check fmt

dev:
	vercel dev

test:
	go test ./...

vet:
	go vet ./...

build:
	go build ./...

check: test vet build

fmt:
	test -z "$$(gofmt -l .)"
