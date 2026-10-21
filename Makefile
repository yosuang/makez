SHELL := /bin/bash

.PHONY: dev clean

dev:
	./dev/dev.sh

clean:
	go run ./build clean

# Pin the generator independently of runtime packages; output has no timestamp.
SWAG = go run github.com/swaggo/swag/cmd/swag@v1.16.6
.PHONY: swagger swagger-check
swagger:
	$(SWAG) init -g internal/api/server.go --parseInternal --packageName docs --propertyStrategy pascalcase --output internal/api/docs --outputTypes go,json,yaml

swagger-check:
	@tmp=$$(mktemp -d); trap 'rm -rf "$$tmp"' EXIT; \
	$(SWAG) init -g internal/api/server.go --parseInternal --packageName docs --propertyStrategy pascalcase --output "$$tmp" --outputTypes go,json,yaml && \
	diff -ru internal/api/docs "$$tmp"
