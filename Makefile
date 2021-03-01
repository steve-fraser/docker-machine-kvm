PREFIX=docker-machine-driver-kvm
MACHINE_VERSION=v0.15.0-rancher51
GO_VERSION=1.15
DESCRIBE=$(shell git describe --tags)

TARGETS=$(addprefix $(PREFIX)-, unraid)

build: $(TARGETS)

$(PREFIX)-%: Dockerfile.%
	docker rmi -f $@ >/dev/null  2>&1 || true
	docker rm -f $@-extract > /dev/null 2>&1 || true
	echo "Building binaries for $@"
	docker build --build-arg "MACHINE_VERSION=$(MACHINE_VERSION)" --build-arg "GO_VERSION=$(GO_VERSION)" -t $@ -f $< .
	docker create --name $@-extract $@ sh
	docker cp $@-extract:/go/bin/docker-machine-driver-kvm ./
	mv ./docker-machine-driver-kvm ./$@
	docker rm $@-extract || true
	docker rmi $@ || true

clean:
	rm -f ./$(PREFIX)-*


release: build
	@echo "Paste the following into the release page on github and upload the binaries..."
	@echo ""
	@for bin in $(PREFIX)-* ; do \
	    target=$$(echo $${bin} | cut -f5- -d-) ; \
	    md5=$$(md5sum $${bin}) ; \
	    echo "* $${target} - md5: $${md5}" ; \
	    echo '```' ; \
	    echo "  curl -L https://github.com/steve-fraser/docker-machine-kvm/releases/download/$(DESCRIBE)/$${bin} > /usr/local/bin/$(PREFIX) \\ " ; \
	    echo "  chmod +x /usr/local/bin/$(PREFIX)" ; \
	    echo '```' ; \
	done

