.PHONY: check test image
check:
	gofmt -w cmd internal
	go vet ./...
	go test -race ./...
	./scripts/contract-test.sh
	PYTHONDONTWRITEBYTECODE=1 python3 .github/docker_release_test.py
test:
	go test ./...
image:
	docker build --platform "$${PLATFORM:-linux/amd64}" -t easy-service:local .
