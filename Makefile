.PHONY: gen build test
gen:
	buf generate
build:
	go build ./...
test:
	go test ./...
