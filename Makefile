.PHONY: build fmt check test-integration test-acceptance docs

build:
	go build -o bin/terraform-provider-dynadot .

fmt:
	gofmt -w main.go internal
	terraform fmt -recursive examples

check:
	test -z "$$(gofmt -l main.go internal)"
	terraform fmt -check -recursive examples
	go vet ./...
	CGO_ENABLED=1 go test -race -shuffle=on ./...
	go build ./...

test-integration:
	CGO_ENABLED=1 go test -race -shuffle=on -count=1 -tags=integration ./internal/provider -timeout 10m

test-acceptance:
	TF_ACC=1 CGO_ENABLED=1 go test -race -tags=acceptance ./internal/provider -run TestAcc -timeout 20m

docs:
	go generate ./...
