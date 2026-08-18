.PHONY: build push docs

REPOSITORY ?= containers.renci.org/helxplatform/ldap-sync
TAG ?= v4.2.1

build: docs
	docker build --platform=linux/amd64 -t $(REPOSITORY):$(TAG) .

push:
	docker push $(REPOSITORY):$(TAG)

docs:
	swag init -g main.go --output ./docs
