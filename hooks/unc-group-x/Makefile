REPO ?= containers.renci.org/helxplatform/unc-group-x
TAG ?= v2.2.1
PLATFORM ?= linux/amd64

.PHONY: docs build push

docs:
	swag init -g main.go

build: docs
	docker build --platform $(PLATFORM) -t $(REPO):$(TAG) .

push:
	docker push $(REPO):$(TAG)
