.PHONY: check test image
check:
	gofmt -w cmd internal
	go vet ./...
	go test -race ./...
	./scripts/contract-test.sh
test:
	go test ./...
image:
	docker build --platform "$${PLATFORM:-linux/amd64}" -t easy-service:local .

