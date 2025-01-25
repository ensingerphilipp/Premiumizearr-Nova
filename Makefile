.POSIX:
.SUFFIXES:

SERVICE = premiumizearrd
GO = go
RM = rm
GOFLAGS =
PREFIX = /usr/local
BUILDDIR = build

all: clean build

web: deps build/web

deps:
	cd web && npm i
	go mod download

build: deps	build/web build/app
	
build/app:
	CGO_ENABLED=0 go build -tags 'netgo osusergo' -ldflags '-extldflags' -o $(BUILDDIR)/$(SERVICE) ./cmd/$(SERVICE)
	cp build/premiumizearrd premiumizearrd

build/web:
	mkdir -p build
	cd web && npm run build
	mkdir -p build/static/ && cp -r web/dist/* build/static/
	cp init/* build/

clean:
	$(RM) -rf build
