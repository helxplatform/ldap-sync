REPO ?= containers.renci.org/helxplatform/ordrd-group-x
TAG ?= v1.1.1
PLATFORM ?= linux/amd64

.PHONY: docs build push

docs:
	swag init -g main.go

build: docs
	docker build --platform $(PLATFORM) -t $(REPO):$(TAG) .

push:
	docker push $(REPO):$(TAG)
