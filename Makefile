.PHONY: build test install clean validate

build:
	go build -o barmkin .

test:
	go test ./... -v

install: build
	sudo ./install.sh

validate: build
	./barmkin validate

clean:
	rm -f barmkin
