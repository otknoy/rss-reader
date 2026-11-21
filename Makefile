REGISTRY ?= ghcr.io
IMAGE := $(REGISTRY)/otknoy/rss-reader

.PHONY: publish
publish:
	@set -eu; \
	tag=$$(date -u +%Y%m%d%H%M%S); \
	revision=$$(git rev-parse HEAD); \
	image="$(IMAGE):$$tag"; \
	docker build --target runtime --label "org.opencontainers.image.revision=$$revision" --tag "$$image" .; \
	docker push "$$image"; \
	printf 'Published %s\n' "$$image"
